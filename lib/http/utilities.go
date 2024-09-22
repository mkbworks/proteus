package http

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"
	"io"
	"regexp"
	"encoding/json"
)

func NewRequest(Connection net.Conn) *HttpRequest {
	var httpRequest HttpRequest
	httpRequest.Initialize()
	reader := bufio.NewReader(Connection)
	httpRequest.setReader(reader)
	return &httpRequest
}

func NewResponse(Connection net.Conn) *HttpResponse {
	var httpResponse HttpResponse
	httpResponse.Initialize()
	writer := bufio.NewWriter(Connection)
	httpResponse.setWriter(writer)
	return &httpResponse
}

func NewServer(ServerHost string) (*HttpServer, error) {
	var server HttpServer
	logger := log.New(os.Stdout, "", log.Ldate | log.Ltime)
	server.SrvLogger = logger
	if ServerHost == "" {
		return nil, errors.New("server host address cannot be empty")
	} else {
		server.HostAddress = ServerHost
	}
	
	server.PortNumber = 0
	server.AllowedContentTypes = make(map[string]string)
	ctFile, err := os.Open("./assets/contenttypes.json")
	if err != nil {
		return nil, err;
	}
	
	defer ctFile.Close()
	fileContents, err := io.ReadAll(ctFile)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(fileContents, &server.AllowedContentTypes)
	if err != nil {
		return nil, err
	}
	return &server, nil
}

func getRfc1123Time() string {
	currentTime := time.Now().UTC()
	return currentTime.Format(time.RFC1123)
}

func getW3CLogLine(req *HttpRequest, res *HttpResponse, ClientAddress string) string {
	return fmt.Sprintf("%s %s %s %d %s", ClientAddress, req.Method, req.ResourcePath, res.StatusCode, req.Version)
}

func getResponseVersion(requestVersion string) string {
	isCompatible := false
	for _, version := range COMPATIBLE_VERSIONS {
		if strings.EqualFold(version, requestVersion) {
			isCompatible = true
			break
		}
	}

	if isCompatible {
		return requestVersion
	} else {
		return MAX_VERSION
	}
}

func validateRoute(Route string) bool {
	if strings.HasPrefix(Route, "//") || !strings.HasPrefix(Route, "/") {
		return false
	}

	RouteName := strings.TrimPrefix(Route, "/")
	isRouteValid, err := regexp.MatchString(VALIDATE_ROUTE_PATTERN, RouteName)
	if err != nil {
		return false
	}

	if !isRouteValid {
		return false
	}
	
	return true
}