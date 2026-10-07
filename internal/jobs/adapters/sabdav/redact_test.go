package sabdav

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestTransportErrorsDoNotLeakAPIKey(t *testing.T) {
	// A port that refuses connections.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	const key = "s3cr3t-api-key"
	a, err := New(Config{APIURL: "http://" + addr, APIKey: key, DavURL: "http://" + addr, Category: "mountenant", MaxAttempts: 1}, AltMount, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = a.Ping(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("API key leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "apikey=REDACTED") {
		t.Fatalf("unexpected error text: %v", err)
	}
}
