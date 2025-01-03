package http

import (
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Number of CPUs that can be used for facilitating concurrency.
var numCPU = runtime.NumCPU()

// Structure to track all the active connections maintained by the server.
type ConnectionWatcher struct {
	// Mutex to synchronize read-write activities for tracking the number of active connections.
	mu sync.RWMutex
	// Contains the number of active connections with the server.
	connCount int
}

// Increases the connection count by the specified delta.
func (cw *ConnectionWatcher) UpdateCount(delta int) {
	cw.mu.Lock()
	cw.connCount += delta
	cw.mu.Unlock()
}

// Returns the connection count value for the ConnectionWatcher instance..
func (cw *ConnectionWatcher) GetCount() int {
	cw.mu.RLock()
	count := cw.connCount
	cw.mu.RUnlock()
	return count
}

// Structure to create an instance of a web server.
type HttpServer struct {
	// Hostname of the web server instance.
	HostAddress string
	// Port number where web server instance is listening for incoming requests.
	PortNumber int
	// Listener created and bound to the host address and port number.
	listener net.Listener
	// Router instance that contains all the routes and their associated handlers.
	innerRouter *Router
	// Logger instance associated with the Server instance.
	eventLogger *logger
	// Waitgroup to synchronize the termination of all request connections.
	wg sync.WaitGroup
	// Channel to transmit server shutdown signal across all goroutines.
	shutdown chan struct{}
	// Instance of ConnectionWatcher.
	cw *ConnectionWatcher
	// Flag to determine if the listener is closed.
	listClosed bool
	// Mutex to manage read-write activities on the listClosed flag.
	limu sync.RWMutex
}

// Function that closes the server listener and marks the listClosed flag as closed.
func (srv *HttpServer) close() {
	srv.limu.Lock()
	srv.listClosed = true
	srv.limu.Unlock()
	srv.listener.Close()
}

// Returns true if the server listener is already closed and false, otherwise.
func (srv *HttpServer) isClosed() bool {
	isClose := false
	srv.limu.RLock()
	isClose = srv.listClosed
	srv.limu.RUnlock()
	return isClose
}

// Define a static route and map to a static file or folder in the file system.
func (srv *HttpServer) Static(Route string, TargetPath string) error {
	err := srv.innerRouter.addStaticRoute("GET", Route, TargetPath)
	if err != nil {
		return err
	}

	err = srv.innerRouter.addStaticRoute("HEAD", Route, TargetPath)
	if err != nil {
		return err
	}

	return nil
}

// Setup the web server instance to listen for incoming HTTP requests at the given hostname and port number.
func (srv * HttpServer) Listen(PortNumber int, HostAddress string) {
	if PortNumber == 0 {
		srv.PortNumber = getDefaultPort()
	} else {
		srv.PortNumber = PortNumber
	}

	if HostAddress == "" {
		srv.HostAddress = getServerDefaults("hostname")
	} else {
		srv.HostAddress = strings.TrimSpace(HostAddress)
	}

	serverAddress := srv.HostAddress + ":" + strconv.Itoa(srv.PortNumber)
	server, err := net.Listen("tcp", serverAddress)
	if err != nil {
		srv.LogError(fmt.Sprintf("Error occurred while setting up listener socket: %s", err.Error()))
		return
	}

	srv.listener = server
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	if srv.shutdown == nil {
		srv.shutdown = make(chan struct{})
	}

	srv.cw = new(ConnectionWatcher)
	srv.LogInfo(fmt.Sprintf("Web server is listening at http://%s", serverAddress))
	srv.LogInfo("To terminate the server, press Ctrl + C")
	srv.wg.Add(1)
	go srv.acceptConnections()
	<-sigChan
	srv.terminate()
	close(sigChan)
}

// Accepts incoming connections and creates seperate goroutines for each new client.
func (srv *HttpServer) acceptConnections() {
	defer srv.wg.Done()
	
	for {
		select {
		case <-srv.shutdown:
			srv.LogInfo("Server Shutdown initiated :: No new connections will be accepted from now.")
			return
		default:
			clientConnection, err := srv.listener.Accept()
			if err != nil {
				if !srv.isClosed() {
					srv.LogError(fmt.Sprintf("Error occurred while accepting a new client: %s", err.Error()))
				}
				continue
			}

			srv.LogInfo(fmt.Sprintf("A new client - %s has connected to the server", clientConnection.RemoteAddr().String()))
			srv.wg.Add(1)
			go srv.handleClient(clientConnection)
			srv.cw.UpdateCount(1)
		}
	}
}

// Handles incoming HTTP requests sent from each individual client trying to connect to the web server instance.
func (srv *HttpServer) handleClient(ClientConnection net.Conn) {
	defer srv.wg.Done()
	defer srv.cw.UpdateCount(-1)
	defer ClientConnection.Close()

	handleRequest := func() (int, error) {
		httpRequest := newRequest(ClientConnection)
		err := httpRequest.read()
		if err != nil {
			_, ok := err.(*ReadTimeoutError)
			if err != io.EOF && !ok {
				srv.LogError(err.Error())
			}
			
			return 0, err
		}

		srv.LogInfo(fmt.Sprintf("Client [%s] :: New Request :: %s %s HTTP/%s", ClientConnection.RemoteAddr().String(), httpRequest.Method, httpRequest.ResourcePath, httpRequest.Version))
		httpResponse := newResponse(ClientConnection, httpRequest)
		var timeout int
		var max int
		connValue, ok := httpRequest.Headers.Get("Connection")
		if ok && strings.EqualFold(connValue, "keep-alive") && strings.EqualFold(httpResponse.Version, "1.1") {
			currCount := srv.cw.GetCount()
			timeout, max = srv.getKeepAliveHeuristic(currCount)
			srv.LogInfo(fmt.Sprintf("The timeout value returned by heuristic is %d seconds for active connection count %d", timeout, currCount))
			tcpConn, ok := ClientConnection.(*net.TCPConn)
			if ok {
				tcpConn.SetKeepAlive(true)
				tcpConn.SetKeepAlivePeriod(time.Duration(timeout) * time.Second)
				tcpConn.SetReadDeadline(time.Now().Add(time.Duration(timeout) * time.Second))
			}
			
			httpResponse.Headers.Add("Connection", "keep-alive")
			httpResponse.Headers.Add("Keep-Alive", fmt.Sprintf("timeout=%d, max=%d", timeout, max))
		}

		if !isMethodAllowed(httpResponse.Version, strings.ToUpper(strings.TrimSpace(httpRequest.Method))) {
			httpResponse.Status(StatusMethodNotAllowed)
			err = ErrorHandler(httpRequest, httpResponse)
			if err != nil {
				srv.LogError(err.Error())
			}
		} else {
			routeHandler, err := srv.innerRouter.matchRoute(httpRequest)
			if err != nil {
				srv.LogError(err.Error())
				httpResponse.Status(StatusNotFound)
				err = ErrorHandler(httpRequest, httpResponse)
				if err != nil {
					srv.LogError(err.Error())
				}
			} else {
				err = routeHandler(httpRequest, httpResponse)
				if err != nil {
					srv.LogError(err.Error())
				}
			}
		}

		srv.Log(httpRequest, httpResponse)
		return timeout, nil
	}

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		timeout, err := handleRequest();
		_, ok := err.(*ReadTimeoutError)
		if err != io.EOF && !ok {
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(time.Duration(timeout) * time.Second)
			srv.LogInfo(fmt.Sprintf("Timeout for client connection [%s] has been changed to %d seconds.", ClientConnection.RemoteAddr().String(), timeout))
		}

		select {
		case <-srv.shutdown:
			srv.LogInfo("Server shutdown initiated :: Closing client connection - " + ClientConnection.RemoteAddr().String())
			return
		case <-timer.C:
			srv.LogInfo(fmt.Sprintf("Client connection [%s] has timed out.", ClientConnection.RemoteAddr().String()))
			return
		default:
		}
	}
}

// Server's Keep-Alive heuristic which returns the timeout value and the maximum number of requests that can be processed by a single connection.
func (srv *HttpServer) getKeepAliveHeuristic(connCount int) (int, int) {
	usableCPU := numCPU - 1
	scalingFactor := 2.0
	timeout := 15 / (1 + math.Exp(scalingFactor * float64(connCount - usableCPU)))
	return int(math.Ceil(timeout)), 100
}

// Terminate all the active connections with the server before shutting down the server instance.
func (srv *HttpServer) terminate() {
	srv.LogInfo("Server shutdown signal received...")
	terminateDone := make(chan struct{})
	go func () {
		srv.LogInfo("Server Shutdown :: All existing connections are being terminated.")
		close(srv.shutdown)
		srv.close()
		srv.wg.Wait()
		close(terminateDone)
	}()

	srvShutTimeout := getSrvShutdownTimout()

	select {
	case <-terminateDone:
		srv.LogInfo("Server Shutdown :: All active connections have been terminated successfully.")
		return
	case <-time.After(time.Duration(srvShutTimeout) * time.Second):
		srv.LogInfo("Server Shutdown Timeout :: Not all active connection(s) were terminated successfully.")
		return
	}
}

// Creates a new GET endpoint at the given route path and sets the handler function to be invoked when the route is requested by the user.
func (srv *HttpServer) Get(routePath string, handlerFunc Handler) error {
	routePath = strings.TrimSpace(routePath)
	err := srv.innerRouter.addDynamicRoute("GET", routePath, handlerFunc)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new HEAD endpoint at the given route path and sets the handler function to be invoked when the route is requested by the user.
func (srv *HttpServer) Head(routePath string, handlerFunc Handler) error {
	routePath = strings.TrimSpace(routePath)
	err := srv.innerRouter.addDynamicRoute("HEAD", routePath, handlerFunc)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new POST endpoint at the given route path and sets the handler function to be invoked when the route is requested by the user.
func (srv *HttpServer) Post(routePath string, handlerFunc Handler) error {
	routePath = strings.TrimSpace(routePath)
	err := srv.innerRouter.addDynamicRoute("POST", routePath, handlerFunc)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new PUT endpoint at the given route path and sets the handler function to be invoked when the route is requested by the user.
func (srv *HttpServer) Put(routePath string, handlerFunc Handler) error {
	routePath = strings.TrimSpace(routePath)
	err := srv.innerRouter.addDynamicRoute("PUT", routePath, handlerFunc)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new DELETE endpoint at the given route path and sets the handler function to be invoked when the route is requested by the user.
func (srv *HttpServer) Delete(routePath string, handlerFunc Handler) error {
	routePath = strings.TrimSpace(routePath)
	err := srv.innerRouter.addDynamicRoute("DELETE", routePath, handlerFunc)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new TRACE endpoint at the given route path and sets the handler function to be invoked when the route is requested by the user.
func (srv *HttpServer) Trace(routePath string, handlerFunc Handler) error {
	routePath = strings.TrimSpace(routePath)
	err := srv.innerRouter.addDynamicRoute("TRACE", routePath, handlerFunc)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new OPTIONS endpoint at the given route path and sets the handler function to be invoked when the route is requested by the user.
func (srv *HttpServer) Options(routePath string, handlerFunc Handler) error {
	routePath = strings.TrimSpace(routePath)
	err := srv.innerRouter.addDynamicRoute("OPTIONS", routePath, handlerFunc)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new CONNECT endpoint at the given route path and sets the handler function to be invoked when the route is requested by the user.
func (srv *HttpServer) Connect(routePath string, handlerFunc Handler) error {
	routePath = strings.TrimSpace(routePath)
	err := srv.innerRouter.addDynamicRoute("CONNECT", routePath, handlerFunc)
	if err != nil {
		return err
	}

	return nil
}

// Logs the given message as an error in the server logs.
func (srv *HttpServer) LogError(message string) {
	message = strings.TrimSpace(message)
	srv.eventLogger.logError(message)
}

// Logs the given message as an information in the server logs.
func (srv *HttpServer) LogInfo(message string) {
	message = strings.TrimSpace(message)
	srv.eventLogger.logInfo(message)
}

// Logs the status for a HTTP request to the server logger.
func (srv *HttpServer) Log(request *HttpRequest, response *HttpResponse) {
	logMsg := fmt.Sprintf("  %s  %s  %s  HTTP/%s  %d  %s", request.ClientAddress, request.Method, request.ResourcePath, request.Version, response.StatusCode, response.StatusMessage)
	if response.StatusCode < 400 {
		srv.eventLogger.logInfo(logMsg)
	} else {
		srv.eventLogger.logError(logMsg)
	}
}