package amcrest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseDigestChallenge(t *testing.T) {
	h := `Digest realm="Login", nonce="abc", qop="auth", opaque="xyz"`
	parts := parseDigestChallenge(h)
	if parts["realm"] != "Login" || parts["nonce"] != "abc" || parts["qop"] != "auth" || parts["opaque"] != "xyz" {
		t.Fatalf("unexpected digest parts: %#v", parts)
	}
}

func TestParseKV(t *testing.T) {
	kv := parseKV(strings.NewReader("deviceType=AD410\nserialNumber=ABC123\n"))
	if kv["deviceType"] != "AD410" || kv["serialNumber"] != "ABC123" {
		t.Fatalf("unexpected kv parse: %#v", kv)
	}
}

func TestParseEventLine(t *testing.T) {
	ev, ok := parseEventLine("Code=_DoorbellPress_;action=Start;index=0;data={}")
	if !ok {
		t.Fatal("expected parse success")
	}
	if ev.Code != "_DoorbellPress_" || ev.Action != "Start" || ev.Index != "0" {
		t.Fatalf("unexpected event parse: %#v", ev)
	}
}

func TestParseEventCodes(t *testing.T) {
	codes := parseEventCodes(strings.NewReader("events[0]=VideoMotion\nevents[1]=_DoorbellPress_\nevents[2]=VideoMotion\n"))
	if len(codes) != 2 {
		t.Fatalf("expected 2 unique codes, got %#v", codes)
	}
	if codes[0] != "VideoMotion" || codes[1] != "_DoorbellPress_" {
		t.Fatalf("unexpected codes: %#v", codes)
	}
}

func TestGetWithDigestHandshake(t *testing.T) {
	challenge := `Digest realm="Login", nonce="abc", qop="auth", opaque="xyz"`
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "deviceType=AD410\n")
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "admin", "password", ts.Client())
	resp, err := c.getWithDigest(context.Background(), "/cgi-bin/magicBox.cgi?action=getSystemInfo")
	if err != nil {
		t.Fatalf("getWithDigest failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if calls < 2 {
		t.Fatalf("expected at least 2 calls for digest handshake, got %d", calls)
	}
}

func TestGetEventCodes(t *testing.T) {
	challenge := `Digest realm="Login", nonce="abc", qop="auth", opaque="xyz"`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "events[0]=VideoMotion\nevents[1]=CrossLineDetection\nevents[2]=_DoorbellPress_\n")
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "admin", "password", ts.Client())
	codes, err := c.GetEventCodes(context.Background())
	if err != nil {
		t.Fatalf("GetEventCodes error: %v", err)
	}
	if len(codes) != 3 {
		t.Fatalf("expected 3 codes, got %#v", codes)
	}
}
