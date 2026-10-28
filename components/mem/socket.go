package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
)

const defaultSocket = "/run/panda-mem/sock"

// Serve is the single writer: it owns the repo and speaks the same
// newline-delimited MCP JSON-RPC as `mcp`, one session per connection.
// Writes from all connections are serialised by the flock in Write.
func (s *Store) Serve(path string) error {
	if err := s.Init(); err != nil {
		return err
	}
	if err := s.checkRepo(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := os.Chmod(path, 0o660); err != nil {
		return err
	}
	log.Printf("panda-mem: serving %s on %s", s.Dir, path)
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			if err := s.ServeMCP(c, c); err != nil {
				log.Printf("panda-mem: session: %v", err)
			}
		}()
	}
}

// proxy pipes an MCP stdio session to the socket server.
func proxy(path string, in io.Reader, out io.Writer) error {
	c, err := net.Dial("unix", path)
	if err != nil {
		return err
	}
	defer c.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		io.Copy(c, in)
		c.(*net.UnixConn).CloseWrite()
	}()
	_, err = io.Copy(out, c)
	return err
}

// remoteTool runs one tools/call over the socket and returns its text.
func remoteTool(path, name string, args toolArgs) (string, error) {
	c, err := net.Dial("unix", path)
	if err != nil {
		return "", err
	}
	defer c.Close()
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	if _, err := c.Write(append(req, '\n')); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	if !sc.Scan() {
		return "", fmt.Errorf("no reply from %s: %v", path, sc.Err())
	}
	var resp struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
		Error *rpcErr `json:"error"`
	}
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return "", err
	}
	if resp.Error != nil {
		return "", errors.New(resp.Error.Message)
	}
	text := ""
	if len(resp.Result.Content) > 0 {
		text = resp.Result.Content[0].Text
	}
	if resp.Result.IsError {
		return "", errors.New(text)
	}
	return text, nil
}
