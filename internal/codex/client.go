package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("Codex RPC %d: %s", e.Code, e.Message) }

// Client multiplexes JSONL responses and server-initiated requests. It owns
// the supplied streams; closing it unblocks a stalled peer and pending calls.
type Client struct {
	reader    io.Reader
	writer    io.Writer
	writeMu   sync.Mutex
	mu        sync.Mutex
	sequence  uint64
	pending   map[string]chan Message
	err       error
	events    chan Message
	done      chan struct{}
	readDone  chan struct{}
	closeOnce sync.Once
}

func NewClient(reader io.Reader, writer io.Writer) *Client {
	c := &Client{reader: reader, writer: writer, pending: make(map[string]chan Message), events: make(chan Message, 128), done: make(chan struct{}), readDone: make(chan struct{})}
	go c.read()
	return c
}

func (c *Client) read() {
	defer func() { close(c.events); close(c.readDone) }()
	scanner := bufio.NewScanner(c.reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var message Message
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			c.fail(fmt.Errorf("invalid Codex JSONL frame: %w", err))
			return
		}
		if message.Method == "" {
			if len(message.ID) == 0 || (message.Result == nil && message.Error == nil) {
				c.fail(errors.New("invalid Codex response envelope"))
				return
			}
			c.mu.Lock()
			receiver := c.pending[string(message.ID)]
			c.mu.Unlock()
			if receiver != nil {
				select {
				case receiver <- message:
				default:
					// Duplicate responses must not strand the stream reader.
				}
			}
			continue
		}
		select {
		case c.events <- message:
		case <-c.done:
			// Ignore terminal progress but drain already-buffered frames so a
			// later correlated response cannot be discarded during shutdown.
			continue
		}
	}
	if err := scanner.Err(); err != nil {
		c.fail(fmt.Errorf("read Codex transport: %w", err))
	} else {
		c.fail(io.EOF)
	}
}

func (c *Client) fail(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
		if closer, ok := c.reader.(io.Closer); ok {
			_ = closer.Close()
		}
		if closer, ok := c.writer.(io.Closer); ok {
			_ = closer.Close()
		}
	})
}

func (c *Client) Err() error             { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
func (c *Client) Done() <-chan struct{}  { return c.done }
func (c *Client) Events() <-chan Message { return c.events }
func (c *Client) Close() error           { c.fail(errors.New("Codex transport closed")); return nil }

func (c *Client) send(message Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return c.Err()
	default:
	}
	return json.NewEncoder(c.writer).Encode(message)
}

func (c *Client) Notify(method string, params any) error {
	return c.NotifyContext(context.Background(), method, params)
}

func (c *Client) sendContext(ctx context.Context, message Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	written := make(chan error, 1)
	go func() { written <- c.send(message) }()
	select {
	case err := <-written:
		return err
	case <-ctx.Done():
		// A partial JSONL write cannot safely be resumed. Close owned streams
		// so both this write and writers queued behind it are unblocked.
		c.fail(ctx.Err())
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
}

func (c *Client) NotifyContext(ctx context.Context, method string, params any) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.sendContext(ctx, Message{Method: method, Params: encoded})
}

func (c *Client) Reply(id json.RawMessage, result any) error {
	return c.ReplyContext(context.Background(), id, result)
}

func (c *Client) ReplyContext(ctx context.Context, id json.RawMessage, result any) error {
	if len(id) == 0 || !json.Valid(id) {
		return errors.New("server request has no valid ID")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.sendContext(ctx, Message{ID: id, Result: encoded})
}

func (c *Client) Reject(id json.RawMessage, err *RPCError) error {
	return c.RejectContext(context.Background(), id, err)
}

func (c *Client) RejectContext(ctx context.Context, id json.RawMessage, err *RPCError) error {
	if len(id) == 0 || !json.Valid(id) {
		return errors.New("server request has no valid ID")
	}
	return c.sendContext(ctx, Message{ID: id, Error: err})
}

func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.sequence++
	id := strconv.FormatUint(c.sequence, 10)
	receiver := make(chan Message, 1)
	c.pending[id] = receiver
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	finishTerminal := func(transportErr error) error {
		// Closing an owned stream unblocks its reader. Wait for that reader
		// to dispatch any complete buffered response before dropping the
		// correlation; context cancellation remains independently bounded.
		select {
		case <-c.readDone:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case message := <-receiver:
			return decodeResponse(method, message, result)
		default:
			return transportErr
		}
	}
	written := make(chan error, 1)
	go func() { written <- c.send(Message{ID: json.RawMessage(id), Method: method, Params: encoded}) }()
	select {
	case err := <-written:
		if err != nil {
			c.fail(err)
			return finishTerminal(err)
		}
	case <-ctx.Done():
		c.fail(ctx.Err())
		return ctx.Err()
	case <-c.done:
		return finishTerminal(c.Err())
	}
	select {
	case message := <-receiver:
		return decodeResponse(method, message, result)
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return finishTerminal(c.Err())
	}
}

func decodeResponse(method string, message Message, result any) error {
	if message.Error != nil {
		return message.Error
	}
	if result != nil {
		if err := json.Unmarshal(message.Result, result); err != nil {
			return fmt.Errorf("decode %s response: %w", method, err)
		}
	}
	return nil
}
