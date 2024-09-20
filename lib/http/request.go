package http

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

type HttpRequest struct {
	Method string
	ResourcePath string
	Version string
	Headers Headers
	Body []byte
	ContentLength int
	reader *bufio.Reader
}

func (req *HttpRequest) Initialize() {
	req.Body = make([]byte, 0)
	req.Version = MAX_VERSION
	req.Headers = make(Headers)
}

func (req *HttpRequest) setReader(reader *bufio.Reader) {
	req.reader = reader
}

func (req *HttpRequest) Read() error {
	err := req.readHeader()
	if err != nil {
		return err
	}

	clength, ok := req.Headers.Get("Content-Length")
	if ok {
		req.ContentLength, err = strconv.Atoi(clength)
		if err != nil {
			return err
		}

		err = req.readBody()
		if err != nil {
			return err
		}
	}

	return nil
}

func (req *HttpRequest) readHeader() error {
	RequestLineProcessed := false
	HeaderProcessingCompleted := false

	for {
		message, err := req.reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		message = strings.TrimSuffix(message, HEADER_LINE_SEPERATOR)
		if len(message) == 0 && !HeaderProcessingCompleted {
			HeaderProcessingCompleted = true
			break
		} else if !RequestLineProcessed {
			RequestLineParts := strings.Split(message, REQUEST_LINE_SEPERATOR)
			if len(RequestLineParts) != 3 {
				return errors.New("request line should contain exactly three values seperated by a single whitespace")
			}
			req.Method = strings.TrimSpace(RequestLineParts[0])
			req.ResourcePath = strings.TrimSpace(RequestLineParts[1])
			req.Version = strings.TrimSpace(RequestLineParts[2])
			RequestLineProcessed = true
		} else {
			HeaderKey, HeaderValue, found := strings.Cut(message, HEADER_KEY_VALUE_SEPERATOR)
			if !found {
				errorString := fmt.Sprintf("error while processing header: %s :: Semicolon is missing", message)
				return errors.New(errorString)
			}
			req.Headers.Add(HeaderKey, HeaderValue)
		}
	}

	return nil
}

func (req *HttpRequest) readBody() error {
	if req.ContentLength > 0 {
		req.Body = make([]byte, req.ContentLength)
		for index := 0; index < req.ContentLength; index++ {
			bodyByte, err := req.reader.ReadByte()
			if err != nil {
				return errors.New("unexpected error occurred. Unable to read request body")
			}
			req.Body[index] = bodyByte
		}
	}

	return nil
}

func (req *HttpRequest) isConditionalGet(FileModifiedTime time.Time) bool {
	ok := req.Headers.Contains("If-Modified-Since")
	if !ok {
		return false
	}
	LastModifiedString, ok := req.Headers.Get("If-Modified-Since")
	if !ok {
		return false
	}
	LastModifiedString = strings.TrimSpace(LastModifiedString)
	LastModifiedSince, err := time.Parse(time.RFC1123, LastModifiedString)
	if err != nil {
		return false
	}
	if FileModifiedTime.After(LastModifiedSince) {
		return false
	}
	return true
}