package testsupport

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPipeServerRoundTrip(t *testing.T) {
	s := NewPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(r.Method + " " + r.URL.Path + " " + string(body)))
	}))
	c := &http.Client{Transport: s.Transport}
	for range 3 { // the second and third reuse the connection
		res, err := c.Post(s.URL+"/v1/x", "text/plain", strings.NewReader("hi"))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if string(got) != "POST /v1/x hi" {
			t.Fatalf("got %q", got)
		}
	}
}

// A flushed chunk reaches the client before the handler finishes, as a
// streamed response does on a socket.
func TestPipeServerStreams(t *testing.T) {
	release := make(chan struct{})
	s := NewPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: 1\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("data: 2\n"))
	}))
	res, err := (&http.Client{Transport: s.Transport}).Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	br := bufio.NewReader(res.Body)
	if line, err := br.ReadString('\n'); err != nil || line != "data: 1\n" {
		t.Fatalf("first chunk = %q, %v", line, err)
	}
	close(release)
	if line, err := br.ReadString('\n'); err != nil || line != "data: 2\n" {
		t.Fatalf("second chunk = %q, %v", line, err)
	}
}
