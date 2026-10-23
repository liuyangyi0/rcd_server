//go:build linux

package app

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// A local, mode-0600 Unix socket: no new public unauthenticated HTTP endpoint.
// The publisher already runs as the service owner/root. Business Web auth and
// OPC security remain unchanged. JSON is bounded and every write is CAS-bound.
func (r *Runtime) startReleaseControl() (func(), error) {
	path := filepath.Join(r.options.DataDir, "release-control.sock")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("release control path is not a socket")
		}
		conn, dialErr := net.DialTimeout("unix", path, time.Second)
		if dialErr == nil {
			conn.Close()
			return nil, errors.New("manager release control already active")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, dialErr
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /release", func(w http.ResponseWriter, req *http.Request) {
		v := r.releaseSnapshot()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", strconv.Quote(strconv.FormatUint(v.Revision, 10)))
		json.NewEncoder(w).Encode(v)
	})
	mux.HandleFunc("POST /release", func(w http.ResponseWriter, req *http.Request) {
		var input managerReleaseRequest
		req.Body = http.MaxBytesReader(w, req.Body, 4096)
		defer req.Body.Close()
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&input); err != nil {
			http.Error(w, "invalid release request", 400)
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			http.Error(w, "one JSON request required", 400)
			return
		}
		if req.Header.Get("If-Match") != strconv.Quote(strconv.FormatUint(input.ExpectedRevision, 10)) {
			http.Error(w, "revision required", 409)
			return
		}
		v, err := r.changeRelease(input)
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", strconv.Quote(strconv.FormatUint(v.Revision, 10)))
		json.NewEncoder(w).Encode(v)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	go server.Serve(listener)
	return func() { server.Close() }, nil
}
