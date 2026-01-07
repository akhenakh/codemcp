package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GoplsInstance is the global singleton instance of the running gopls client.
// It is initialized via InitGopls() and accessed by the search logic.
var GoplsInstance *GoplsClient

// GoplsClient manages the lifecycle and communication with a gopls subprocess.
// It implements a basic JSON-RPC 2.0 client over Stdio.
type GoplsClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	seq     int64 // Atomic sequence counter for request IDs
	pending map[int64]chan json.RawMessage
	mu      sync.Mutex
}

// InitGopls starts the gopls process in a background goroutine and performs the
// initial handshake (initialize -> initialized).
// rootPath is the absolute path to the project root, used to set the workspace context.
func InitGopls(rootPath string) {
	// Check if binary exists in PATH
	if _, err := exec.LookPath("gopls"); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️ gopls not found, skipping dependency search\n")
		return
	}

	// Start the subprocess
	cmd := exec.Command("gopls")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	// stderr is intentionally ignored to prevent gopls debug logs from polluting our CLI output,
	// but can be piped to os.Stderr for debugging.

	if err := cmd.Start(); err != nil {
		return
	}

	client := &GoplsClient{
		cmd:     cmd,
		stdin:   stdin,
		pending: make(map[int64]chan json.RawMessage),
	}

	// 3. Start the async reader loop to handle responses
	go client.ReadLoop(stdout)

	// 4. Send the LSP 'initialize' request
	initParams := map[string]any{
		"processId":    os.Getpid(),
		"rootUri":      "file://" + rootPath,
		"capabilities": map[string]any{},
	}

	// Block until initialization is acknowledged
	resp, err := client.Call("initialize", initParams)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Gopls init failed: %v\n", err)
		return
	}

	// 5. Notify the server that we are initialized
	// Note: 'notify' does not expect a response.
	client.Notify("initialized", map[string]any{})

	if resp != nil {
		GoplsInstance = client
	}
}

// ShutdownGopls gracefully kills the underlying gopls process.
func ShutdownGopls() {
	if GoplsInstance != nil && GoplsInstance.cmd != nil {
		_ = GoplsInstance.cmd.Process.Kill()
	}
}

// JsonRpcReq represents an outgoing request.
type JsonRpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// JsonRpcNotification represents an outgoing notification (no ID).
type JsonRpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// JsonRpcResp represents an incoming response.
type JsonRpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Notify sends a JSON-RPC notification (fire and forget).
func (c *GoplsClient) Notify(method string, params any) error {
	msg := JsonRpcNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	return c.write(msg)
}

// Call sends a request and blocks waiting for a response or timeout.
func (c *GoplsClient) Call(method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.seq, 1)
	ch := make(chan json.RawMessage, 1)

	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	req := JsonRpcReq{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	if err := c.write(req); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	// Wait for response or timeout
	select {
	case res := <-ch:
		return res, nil
	case <-time.After(5 * time.Second):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("timeout waiting for gopls response")
	}
}

// write formats the message with LSP Content-Length headers and writes to stdin.
func (c *GoplsClient) write(msg any) error {
	body, _ := json.Marshal(msg)
	// LSP requires Content-Length header followed by \r\n\r\n
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.stdin.Write([]byte(header)); err != nil {
		return err
	}
	_, err := c.stdin.Write(body)
	return err
}

// ReadLoop runs in a background goroutine. It continuously parses headers
// and bodies from the gopls stdout stream.
func (c *GoplsClient) ReadLoop(r io.Reader) {
	reader := bufio.NewReader(r)
	tp := textproto.NewReader(reader)

	for {
		// Read MIME Headers (e.g. Content-Length: 123)
		headers, err := tp.ReadMIMEHeader()
		if err != nil {
			return // Pipe closed or error
		}

		lengthStr := headers.Get("Content-Length")
		length, _ := strconv.Atoi(lengthStr)

		if length == 0 {
			continue
		}

		// Read the exact number of bytes for the Body
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			return
		}

		// Process the message asynchronously to not block reading
		go c.handleMessage(body)
	}
}

// handleMessage dispatches responses to waiting callers via channels.
func (c *GoplsClient) handleMessage(body []byte) {
	var resp JsonRpcResp
	// Try to unmarshal. We only care about responses with IDs.
	if err := json.Unmarshal(body, &resp); err == nil && resp.ID != 0 {
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()

		if ok {
			ch <- resp.Result
		}
	}
	// Note: We currently ignore server-sent notifications (like diagnostics/publishDiagnostics)
}

// SymbolSearch sends a 'workspace/symbol' request to gopls.
// It performs aggressive filtering to reduce noise from the Go standard library
// and internal dependencies.
func (c *GoplsClient) SymbolSearch(query string) ([]FileScore, error) {
	params := map[string]any{
		"query": query,
	}

	res, err := c.Call("workspace/symbol", params)
	if err != nil {
		return nil, err
	}

	var symbols []struct {
		Name          string `json:"name"`
		Kind          int    `json:"kind"` // 12=Function, 5=Struct, etc.
		ContainerName string `json:"containerName"`
		Location      struct {
			URI string `json:"uri"`
		} `json:"location"`
	}

	if err := json.Unmarshal(res, &symbols); err != nil {
		return nil, err
	}

	var results []FileScore
	queryLower := strings.ToLower(query)
	goRoot := runtime.GOROOT() // e.g. /usr/local/go

	for _, s := range symbols {
		// Filter 1: Strict Matching
		// Gopls fuzzy matching is very loose (e.g. "search" matches "TLS_ECDHE...").
		// We enforce contiguous substring matching.
		nameLower := strings.ToLower(s.Name)
		if !strings.Contains(nameLower, queryLower) {
			continue
		}

		// Convert URI (file:///path) to a standard path string
		pathStr := strings.TrimPrefix(s.Location.URI, "file://")

		// Filter 2: Noise Reduction
		// Filter out Go Standard Library and vendor folders.
		if strings.HasPrefix(pathStr, goRoot) ||
			strings.Contains(pathStr, "/vendor/") ||
			strings.Contains(pathStr, "/.cache/") {
			continue
		}

		isDep := strings.Contains(pathStr, "/pkg/mod/")

		// Scoring Logic
		score := 50 // Base score for a gopls match

		// Boost: Exact Match or Prefix Match
		if strings.EqualFold(s.Name, query) {
			score += 50
		} else if strings.HasPrefix(nameLower, queryLower) {
			score += 20
		}

		// Boost: Significant Types (Structs, Functions, Interfaces)
		// LSP Kinds: 5=Class, 11=Function, 12=Method
		if s.Kind == 5 || s.Kind == 11 || s.Kind == 12 {
			score += 10
		}

		// Penalty: Dependencies
		// We want user code to rank higher than library code usually.
		if isDep {
			score -= 25
		}

		// Penalty: Dependency Tests
		// Tests inside dependencies are almost never relevant search results.
		if isDep && strings.HasSuffix(pathStr, "_test.go") {
			score -= 50
		}

		if score <= 0 {
			continue
		}

		results = append(results, FileScore{
			Path:    pathStr,
			Score:   score,
			Reasons: []string{fmt.Sprintf("gopls:%s", s.Name)},
			IsDep:   isDep,
		})
	}

	return results, nil
}
