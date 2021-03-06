package http

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gliderlabs/logspout/router"
)

// RFC3339 with millisecond resolution
const TIME_FORMAT_RFC3339_MS = "2006-01-02T15:04:05.000Z07:00"

var dialTimeout time.Duration

func init() {
	dialTimeout, _ = time.ParseDuration("30s")
	router.AdapterFactories.Register(NewHTTPAdapter, "sumo")
}

func debug(v ...interface{}) {
	if os.Getenv("DEBUG") != "" {
		log.Println(v...)
	}
}

func die(v ...interface{}) {
	panic(fmt.Sprintln(v...))
}

func getStringParameter(
	options map[string]string, parameterName string, dfault string) string {

	if value, ok := options[parameterName]; ok {
		return value
	} else {
		return dfault
	}
}

func getIntParameter(
	options map[string]string, parameterName string, dfault int) int {

	if value, ok := options[parameterName]; ok {
		valueInt, err := strconv.Atoi(value)
		if err != nil {
			debug("http: invalid value for parameter:", parameterName, value)
			return dfault
		} else {
			return valueInt
		}
	} else {
		return dfault
	}
}

func getDurationParameter(
	options map[string]string, parameterName string,
	dfault time.Duration) time.Duration {

	if value, ok := options[parameterName]; ok {
		valueDuration, err := time.ParseDuration(value)
		if err != nil {
			debug("http: invalid value for parameter:", parameterName, value)
			return dfault
		} else {
			return valueDuration
		}
	} else {
		return dfault
	}
}

func dial(netw, addr string) (net.Conn, error) {
	dial, err := net.DialTimeout(netw, addr, dialTimeout)
	if err != nil {
		debug("http: new dial", dial, err, netw, addr)
	} else {
		debug("http: new dial", dial, netw, addr)
	}
	return dial, err
}

// HTTPAdapter is an adapter that POSTs logs to an HTTP endpoint
type HTTPAdapter struct {
	route             *router.Route
	url               string
	client            *http.Client
	buffer            []*router.Message
	timer             *time.Timer
	capacity          int
	timeout           time.Duration
	totalMessageCount int
	bufferMutex       sync.Mutex
	useGzip           bool
	crash             bool
	headers           map[string]string
}

// NewHTTPAdapter creates an HTTPAdapter
func NewHTTPAdapter(route *router.Route) (router.LogAdapter, error) {
	endpointUrl := fmt.Sprintf("https://collectors.sumologic.com/receiver/v1/http/%s", route.Address)
	debug("http: url:", endpointUrl)
	transport := &http.Transport{}
	transport.Dial = dial

	// Figure out if we need a proxy
	defaultProxyUrl := ""
	proxyUrlString := getStringParameter(route.Options, "http.proxy", defaultProxyUrl)
	if proxyUrlString != "" {
		proxyUrl, err := url.Parse(proxyUrlString)
		if err != nil {
			die("", "http: cannot parse proxy url:", err, proxyUrlString)
		}
		transport.Proxy = http.ProxyURL(proxyUrl)
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		debug("http: proxy url:", proxyUrl)
	}

	// Create the client
	client := &http.Client{Transport: transport}

	// Determine the buffer capacity
	defaultCapacity := 100
	capacity := getIntParameter(
		route.Options, "http.buffer.capacity", defaultCapacity)
	if capacity < 1 || capacity > 10000 {
		debug("http: non-sensical value for parameter: http.buffer.capacity",
			capacity, "using default:", defaultCapacity)
		capacity = defaultCapacity
	}
	buffer := make([]*router.Message, 0, capacity)

	// Determine the buffer timeout
	defaultTimeout, _ := time.ParseDuration("1000ms")
	timeout := getDurationParameter(
		route.Options, "http.buffer.timeout", defaultTimeout)
	timeoutSeconds := timeout.Seconds()
	if timeoutSeconds < .1 || timeoutSeconds > 600 {
		debug("http: non-sensical value for parameter: http.buffer.timeout",
			timeout, "using default:", defaultTimeout)
		timeout = defaultTimeout
	}
	timer := time.NewTimer(timeout)

	// Figure out whether we should use GZIP compression
	useGzip := false
	useGZipString := getStringParameter(route.Options, "http.gzip", "true")
	if useGZipString == "true" {
		useGzip = true
		debug("http: gzip compression enabled")
	}

	// Should we crash on an error or keep going?
	crash := true
	crashString := getStringParameter(route.Options, "http.crash", "true")
	if crashString == "false" {
		crash = false
		debug("http: don't crash, keep going")
	}

	headers := make(map[string]string)
	if host := getStringParameter(route.Options, "host", ""); host != "" {
		headers["X-Sumo-Host"] = host
	}
	if name := getStringParameter(route.Options, "name", ""); name != "" {
		headers["X-Sumo-Name"] = name
	}

	// Make the HTTP adapter
	return &HTTPAdapter{
		route:    route,
		url:      endpointUrl,
		client:   client,
		buffer:   buffer,
		timer:    timer,
		capacity: capacity,
		timeout:  timeout,
		useGzip:  useGzip,
		crash:    crash,
		headers:  headers,
	}, nil
}

// Stream implements the router.LogAdapter interface
func (a *HTTPAdapter) Stream(logstream chan *router.Message) {
	for {
		select {
		case message := <-logstream:

			// Append the message to the buffer
			a.bufferMutex.Lock()
			a.buffer = append(a.buffer, message)
			a.bufferMutex.Unlock()

			// Flush if the buffer is at capacity
			if len(a.buffer) >= cap(a.buffer) {
				a.flushHttp("full")
			}
		case <-a.timer.C:

			// Timeout, flush
			a.flushHttp("timeout")
		}
	}
}

// Flushes the accumulated messages in the buffer
func (a *HTTPAdapter) flushHttp(reason string) {

	// Stop the timer and drain any possible remaining events
	a.timer.Stop()
	select {
	case <-a.timer.C:
	default:
	}

	// Reset the timer when we are done
	defer a.timer.Reset(a.timeout)

	// Return immediately if the buffer is empty
	if len(a.buffer) < 1 {
		return
	}

	// Capture the buffer and make a new one
	a.bufferMutex.Lock()
	buffer := a.buffer
	a.buffer = make([]*router.Message, 0, a.capacity)
	a.bufferMutex.Unlock()

	// Create JSON representation of all messages
	messages := make([]string, 0, len(buffer))
	for i := range buffer {
		input := buffer[i]
		var message interface{}

		// attempt to JSON decode the message
		validJSON := true
		err := json.Unmarshal([]byte(input.Data), &message)
		if err != nil {
			validJSON = false
			json.Unmarshal([]byte("{}"), &message)
		}

		messageIfc := message.(map[string]interface{})

		// include the raw message if it wasn't valid JSON
		if !validJSON {
			messageIfc["msg"] = input.Data
		}

		// include the logspout data
		messageIfc["logspout"] = LogspoutData{
			Time:     input.Time.Format(TIME_FORMAT_RFC3339_MS),
			Source:   input.Source,
			Name:     input.Container.Name,
			ID:       input.Container.ID,
			Image:    input.Container.Config.Image,
			Hostname: input.Container.Config.Hostname,
		}

		// save off the message timestamp, preferring the message's
		// own `time` property if set
		timestamp := input.Time.Format(TIME_FORMAT_RFC3339_MS)
		if t, ok := messageIfc["time"]; ok {
			timestamp = t.(string)
		}
		delete(messageIfc, "time")

		messageBuf, err := json.Marshal(message)
		if err != nil {
			debug("flushHttp - Error encoding JSON: ", err)
			continue
		}

		// insert `time` at the head, since Go sorts keys and SumoLogic will use
		// the first string that looks like a timestamp.
		messageStr := "{\"time\":\"" + timestamp + "\"," + string(messageBuf[1:])

		messages = append(messages, messageStr)
	}

	// Glue all the JSON representations together into one payload to send
	payload := strings.Join(messages, "\n")

	go func() {

		// Create the request and send it on its way
		request := createRequest(a.url, a.useGzip, payload, a.headers)
		start := time.Now()
		response, err := a.client.Do(request)
		if err != nil {
			debug("http - error on client.Do:", err, a.url)
			// TODO @raychaser - now what?
			if a.crash {
				die("http - error on client.Do:", err, a.url)
			} else {
				debug("http: error on client.Do:", err)
			}
		}
		if response.StatusCode != 200 {
			debug("http: response not 200 but", response.StatusCode)
			// TODO @raychaser - now what?
			if a.crash {
				die("http: response not 200 but", response.StatusCode)
			}
		}

		// Make sure the entire response body is read so the HTTP
		// connection can be reused
		io.Copy(ioutil.Discard, response.Body)
		response.Body.Close()

		// Bookkeeping, logging
		timeAll := time.Since(start)
		a.totalMessageCount += len(messages)
		debug("http: flushed:", reason, "messages:", len(messages),
			"in:", timeAll, "total:", a.totalMessageCount)
	}()
}

// Create the request based on whether GZIP compression is to be used
func createRequest(url string, useGzip bool, payload string, headers map[string]string) *http.Request {
	var request *http.Request
	if useGzip {
		gzipBuffer := new(bytes.Buffer)
		gzipWriter := gzip.NewWriter(gzipBuffer)
		_, err := gzipWriter.Write([]byte(payload))
		if err != nil {
			// TODO @raychaser - now what?
			die("http: unable to write to GZIP writer:", err)
		}
		err = gzipWriter.Close()
		if err != nil {
			// TODO @raychaser - now what?
			die("http: unable to close GZIP writer:", err)
		}
		request, err = http.NewRequest("POST", url, gzipBuffer)
		if err != nil {
			debug("http: error on http.NewRequest:", err, url)
			// TODO @raychaser - now what?
			die("", "http: error on http.NewRequest:", err, url)
		}
		request.Header.Set("Content-Encoding", "gzip")
	} else {
		var err error
		request, err = http.NewRequest("POST", url, strings.NewReader(payload))
		if err != nil {
			debug("http: error on http.NewRequest:", err, url)
			// TODO @raychaser - now what?
			die("", "http: error on http.NewRequest:", err, url)
		}
	}
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	return request
}

// LogspoutData is a simple JSON representation of the logspout message data.
type LogspoutData struct {
	Time     string `json:"time"`
	Source   string `json:"source"`
	Name     string `json:"docker_name"`
	ID       string `json:"docker_id"`
	Image    string `json:"docker_image"`
	Hostname string `json:"docker_hostname"`
}
