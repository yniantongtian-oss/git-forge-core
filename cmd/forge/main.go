package main

// The entry <-> env lifecycle: bind an ephemeral port, register it with the env, poll until the env says the trial
// is over, then exit 0.
//
//  1. MAKEFUN_ENV_URL names the env's origin. Without it there is nothing to talk to: exit 1.
//  2. Listen on 0.0.0.0:0: the kernel picks a free port (never 8080, which the suite owns).
//  3. POST $MAKEFUN_ENV_URL/register {"port": N}; the first register wins (a 409 is not an error).
//  4. GET $MAKEFUN_ENV_URL/done until {"done": true}, polling with a short backoff.
//  5. Exit 0 (also on SIGINT / SIGTERM).
//
// The env client speaks raw HTTP/1.1 over net.Dial: net/http (TLS, HTTP/2) would double the binary.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const envTimeout = 30 * time.Second

func retryDelay(attempt int) time.Duration {
	return time.Duration(min(2000, 250<<min(attempt, 8))) * time.Millisecond
}

// envRequest sends one request to the env and returns the status and body (Connection: close, no keep-alive).
func envRequest(env *url.URL, method, path string, body []byte) (int, []byte, error) {
	hostPort := env.Host
	if env.Port() == "" {
		hostPort = net.JoinHostPort(env.Hostname(), "80")
	}
	conn, err := net.DialTimeout("tcp", hostPort, envTimeout)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(envTimeout))
	req := method + " " + strings.TrimRight(env.Path, "/") + path + " HTTP/1.1\r\nHost: " + env.Host + "\r\nConnection: close\r\nAccept: application/json\r\n"
	if body != nil {
		req += "Content-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n"
	}
	if _, err := conn.Write(append([]byte(req+"\r\n"), body...)); err != nil {
		return 0, nil, err
	}
	raw, err := io.ReadAll(conn)
	if err != nil && len(raw) == 0 {
		return 0, nil, err
	}
	headEnd := bytes.Index(raw, []byte("\r\n\r\n"))
	if headEnd < 0 || !bytes.HasPrefix(raw, []byte("HTTP/1.")) || len(raw) < 12 {
		return 0, nil, errors.New("malformed response")
	}
	status, err := strconv.Atoi(string(raw[9:12]))
	if err != nil {
		return 0, nil, err
	}
	head := strings.ToLower(string(raw[:headEnd]))
	payload := raw[headEnd+4:]
	if strings.Contains(head, "\r\ntransfer-encoding: chunked") {
		var decoded []byte
		for {
			line := bytes.Index(payload, []byte("\r\n"))
			if line < 0 {
				break
			}
			size, err := strconv.ParseInt(strings.TrimSpace(strings.SplitN(string(payload[:line]), ";", 2)[0]), 16, 64)
			if err != nil || size <= 0 || int64(len(payload)) < int64(line)+2+size {
				break
			}
			decoded = append(decoded, payload[line+2:int64(line)+2+size]...)
			payload = payload[int64(line)+2+size:]
			payload = bytes.TrimPrefix(payload, []byte("\r\n"))
		}
		payload = decoded
	} else if i := strings.Index(head, "\r\ncontent-length:"); i >= 0 {
		value := head[i+len("\r\ncontent-length:"):]
		if end := strings.Index(value, "\r\n"); end >= 0 {
			value = value[:end]
		}
		if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n <= len(payload) {
			payload = payload[:n]
		}
	}
	return status, payload, nil
}

func register(env *url.URL, port int) error {
	body := []byte(`{"port":` + strconv.Itoa(port) + `}`)
	for attempt := 0; ; attempt++ {
		status, reply, err := envRequest(env, "POST", "/register", body)
		if err == nil {
			if status == 200 || status == 409 {
				return nil
			}
			if status == 400 || status == 405 {
				return errors.New("register answered HTTP " + strconv.Itoa(status) + ": " + string(reply))
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func waitForDone(env *url.URL) {
	for attempt := 0; ; attempt++ {
		status, reply, err := envRequest(env, "GET", "/done", nil)
		if err == nil && status >= 200 && status < 300 {
			var answer struct {
				Done any `json:"done"`
			}
			if json.Unmarshal(reply, &answer) == nil && answer.Done == true {
				return
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		os.Stderr.WriteString("[forge] MAKEFUN_ENV_URL is not set: there is no env to register with\n")
		os.Exit(1)
	}
	env, err := url.Parse(envURL)
	if err != nil || env.Host == "" {
		os.Stderr.WriteString("[forge] MAKEFUN_ENV_URL is not a usable URL: " + envURL + "\n")
		os.Exit(1)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		os.Exit(0)
	}()

	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		os.Stderr.WriteString("[forge] " + err.Error() + "\n")
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	os.Stdout.WriteString("[forge] listening on port " + strconv.Itoa(port) + "\n")
	go serveHTTP(ln)

	if err := register(env, port); err != nil {
		os.Stderr.WriteString("[forge] " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("[forge] registered; waiting for the env to say done\n")
	waitForDone(env)
	ln.Close()
	os.Stdout.WriteString("[forge] done\n")
	os.Exit(0)
}
