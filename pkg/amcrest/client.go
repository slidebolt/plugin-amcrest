package amcrest

import (
	"bufio"
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	BaseURL    string
	Username   string
	Password   string
	HTTPClient *http.Client
}

type Event struct {
	Code   string
	Action string
	Index  string
	Data   string
	Time   time.Time
}

func NewClient(baseURL, username, password string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Username: username, Password: password, HTTPClient: httpClient}
}

func (c *Client) GetSystemInfo(ctx context.Context) (map[string]string, error) {
	resp, err := c.getWithDigest(ctx, "/cgi-bin/magicBox.cgi?action=getSystemInfo")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return parseKV(resp.Body), nil
}

func (c *Client) GetSoftwareVersion(ctx context.Context) (string, error) {
	resp, err := c.getWithDigest(ctx, "/cgi-bin/magicBox.cgi?action=getSoftwareVersion")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	kv := parseKV(resp.Body)
	return kv["version"], nil
}

func (c *Client) GetSnapshot(ctx context.Context) ([]byte, error) {
	resp, err := c.getWithDigest(ctx, "/cgi-bin/snapshot.cgi")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *Client) SubscribeEvents(ctx context.Context, codes []string, onEvent func(Event)) error {
	if len(codes) == 0 {
		codes = []string{"All"}
	}
	path := "/cgi-bin/eventManager.cgi?action=attach&codes=[" + strings.Join(codes, ",") + "]"
	resp, err := c.getWithDigest(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("event stream status=%d", resp.StatusCode)
	}
	return parseEventStream(resp.Body, onEvent)
}

func (c *Client) GetEventCodes(ctx context.Context) ([]string, error) {
	resp, err := c.getWithDigest(ctx, "/cgi-bin/eventManager.cgi?action=getEventIndexes&code=All")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("getEventIndexes status=%d", resp.StatusCode)
	}
	return parseEventCodes(resp.Body), nil
}

func (c *Client) GetAllConfig(ctx context.Context) (map[string]string, error) {
	resp, err := c.getWithDigest(ctx, "/cgi-bin/configManager.cgi?action=getConfig&name=All")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("getConfig(All) status=%d", resp.StatusCode)
	}
	return parseKV(resp.Body), nil
}

func (c *Client) getWithDigest(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	_ = resp.Body.Close()

	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Digest") {
		return nil, fmt.Errorf("expected digest challenge")
	}
	params := parseDigestChallenge(challenge)
	auth := buildDigestAuthHeader(c.Username, c.Password, req.Method, req.URL.RequestURI(), params)

	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req2.Header.Set("Authorization", auth)
	return c.HTTPClient.Do(req2)
}

func parseDigestChallenge(header string) map[string]string {
	header = strings.TrimPrefix(header, "Digest ")
	parts := map[string]string{}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(part[:idx])
		val := strings.Trim(strings.TrimSpace(part[idx+1:]), `"`)
		parts[key] = val
	}
	return parts
}

func buildDigestAuthHeader(username, password, method, uri string, params map[string]string) string {
	realm := params["realm"]
	nonce := params["nonce"]
	opaque := params["opaque"]
	qop := params["qop"]

	ha1 := md5hex(username + ":" + realm + ":" + password)
	ha2 := md5hex(method + ":" + uri)
	if strings.Contains(qop, "auth") {
		nc := "00000001"
		cnonce := md5hex(fmt.Sprintf("%d", time.Now().UnixNano()))[:8]
		resp := md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)
		return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", qop=auth, nc=%s, cnonce="%s", response="%s", opaque="%s"`,
			username, realm, nonce, uri, nc, cnonce, resp, opaque)
	}
	resp := md5hex(ha1 + ":" + nonce + ":" + ha2)
	return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", opaque="%s"`,
		username, realm, nonce, uri, resp, opaque)
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return fmt.Sprintf("%x", h)
}

func parseKV(r io.Reader) map[string]string {
	out := map[string]string{}
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		out[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
	}
	return out
}

func parseEventCodes(r io.Reader) []string {
	s := bufio.NewScanner(r)
	out := make([]string, 0, 16)
	seen := map[string]struct{}{}
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if !strings.HasPrefix(key, "events[") {
			continue
		}
		value := strings.TrimSpace(line[idx+1:])
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func parseEventStream(r io.Reader, onEvent func(Event)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	inBody := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "--myboundary") {
			inBody = false
			continue
		}
		if line == "" && !inBody {
			inBody = true
			continue
		}
		if !inBody {
			continue
		}
		ev, ok := parseEventLine(line)
		if ok && onEvent != nil {
			onEvent(ev)
		}
	}
	return scanner.Err()
}

func parseEventLine(line string) (Event, bool) {
	parts := strings.Split(line, ";")
	if len(parts) == 0 {
		return Event{}, false
	}
	fields := map[string]string{}
	for _, part := range parts {
		idx := strings.Index(part, "=")
		if idx < 0 {
			continue
		}
		k := strings.TrimSpace(part[:idx])
		v := strings.TrimSpace(part[idx+1:])
		fields[k] = v
	}
	if fields["Code"] == "" {
		return Event{}, false
	}
	return Event{
		Code:   fields["Code"],
		Action: fields["action"],
		Index:  fields["index"],
		Data:   fields["data"],
		Time:   time.Now().UTC(),
	}, true
}
