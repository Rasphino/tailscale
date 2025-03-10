// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package logtail sends logs to log.tailscale.io.
package logtail

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/logtail/backoff"
	"tailscale.com/net/interfaces"
	tslogger "tailscale.com/types/logger"
	"tailscale.com/wgengine/monitor"
)

// DefaultHost is the default host name to upload logs to when
// Config.BaseURL isn't provided.
const DefaultHost = "log.tailscale.io"

const defaultFlushDelay = 2 * time.Second

const (
	// CollectionNode is the name of a logtail Config.Collection
	// for tailscaled (or equivalent: IPNExtension, Android app).
	CollectionNode = "tailnode.log.tailscale.io"
)

type Encoder interface {
	EncodeAll(src, dst []byte) []byte
	Close() error
}

type Config struct {
	Collection     string           // collection name, a domain name
	PrivateID      PrivateID        // private ID for the primary log stream
	CopyPrivateID  PrivateID        // private ID for a log stream that is a superset of this log stream
	BaseURL        string           // if empty defaults to "https://log.tailscale.io"
	HTTPC          *http.Client     // if empty defaults to http.DefaultClient
	SkipClientTime bool             // if true, client_time is not written to logs
	LowMemory      bool             // if true, logtail minimizes memory use
	TimeNow        func() time.Time // if set, substitutes uses of time.Now
	Stderr         io.Writer        // if set, logs are sent here instead of os.Stderr
	StderrLevel    int              // max verbosity level to write to stderr; 0 means the non-verbose messages only
	Buffer         Buffer           // temp storage, if nil a MemoryBuffer
	NewZstdEncoder func() Encoder   // if set, used to compress logs for transmission

	// MetricsDelta, if non-nil, is a func that returns an encoding
	// delta in clientmetrics to upload alongside existing logs.
	// It can return either an empty string (for nothing) or a string
	// that's safe to embed in a JSON string literal without further escaping.
	MetricsDelta func() string

	// FlushDelay is how long to wait to accumulate logs before
	// uploading them.
	//
	// If zero, a default value is used. (currently 2 seconds)
	//
	// Negative means to upload immediately.
	FlushDelay time.Duration

	// IncludeProcID, if true, results in an ephemeral process identifier being
	// included in logs. The ID is random and not guaranteed to be globally
	// unique, but it can be used to distinguish between different instances
	// running with same PrivateID.
	IncludeProcID bool

	// IncludeProcSequence, if true, results in an ephemeral sequence number
	// being included in the logs. The sequence number is incremented for each
	// log message sent, but is not persisted across process restarts.
	IncludeProcSequence bool
}

func NewLogger(cfg Config, logf tslogger.Logf) *Logger {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://" + DefaultHost
	}
	if cfg.HTTPC == nil {
		cfg.HTTPC = http.DefaultClient
	}
	if cfg.TimeNow == nil {
		cfg.TimeNow = time.Now
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.Buffer == nil {
		pendingSize := 256
		if cfg.LowMemory {
			pendingSize = 64
		}
		cfg.Buffer = NewMemoryBuffer(pendingSize)
	}
	var procID uint32
	if cfg.IncludeProcID {
		keyBytes := make([]byte, 4)
		rand.Read(keyBytes)
		procID = binary.LittleEndian.Uint32(keyBytes)
		if procID == 0 {
			// 0 is the empty/off value, assign a different (non-zero) value to
			// make sure we still include an ID (actual value does not matter).
			procID = 7
		}
	}
	if s := envknob.String("TS_DEBUG_LOGTAIL_FLUSHDELAY"); s != "" {
		var err error
		cfg.FlushDelay, err = time.ParseDuration(s)
		if err != nil {
			log.Fatalf("invalid TS_DEBUG_LOGTAIL_FLUSHDELAY: %v", err)
		}
	} else if cfg.FlushDelay == 0 && !envknob.Bool("IN_TS_TEST") {
		cfg.FlushDelay = defaultFlushDelay
	}

	stdLogf := func(f string, a ...any) {
		fmt.Fprintf(cfg.Stderr, strings.TrimSuffix(f, "\n")+"\n", a...)
	}
	var urlSuffix string
	if !cfg.CopyPrivateID.IsZero() {
		urlSuffix = "?copyId=" + cfg.CopyPrivateID.String()
	}
	l := &Logger{
		privateID:      cfg.PrivateID,
		stderr:         cfg.Stderr,
		stderrLevel:    int64(cfg.StderrLevel),
		httpc:          cfg.HTTPC,
		url:            cfg.BaseURL + "/c/" + cfg.Collection + "/" + cfg.PrivateID.String() + urlSuffix,
		lowMem:         cfg.LowMemory,
		buffer:         cfg.Buffer,
		skipClientTime: cfg.SkipClientTime,
		drainWake:      make(chan struct{}, 1),
		sentinel:       make(chan int32, 16),
		flushDelay:     cfg.FlushDelay,
		timeNow:        cfg.TimeNow,
		bo:             backoff.NewBackoff("logtail", stdLogf, 30*time.Second),
		metricsDelta:   cfg.MetricsDelta,

		procID:              procID,
		includeProcSequence: cfg.IncludeProcSequence,

		shutdownStart: make(chan struct{}),
		shutdownDone:  make(chan struct{}),
	}
	if cfg.NewZstdEncoder != nil {
		l.zstdEncoder = cfg.NewZstdEncoder()
	}

	ctx, cancel := context.WithCancel(context.Background())
	l.uploadCancel = cancel

	go l.uploading(ctx)
	l.Write([]byte("logtail started"))
	return l
}

// Logger writes logs, splitting them as configured between local
// logging facilities and uploading to a log server.
type Logger struct {
	stderr         io.Writer
	stderrLevel    int64 // accessed atomically
	httpc          *http.Client
	url            string
	lowMem         bool
	skipClientTime bool
	linkMonitor    *monitor.Mon
	buffer         Buffer
	drainWake      chan struct{} // signal to speed up drain
	flushDelay     time.Duration // negative or zero to upload agressively, or >0 to batch at this delay
	flushPending   atomic.Bool
	sentinel       chan int32
	timeNow        func() time.Time
	bo             *backoff.Backoff
	zstdEncoder    Encoder
	uploadCancel   func()
	explainedRaw   bool
	metricsDelta   func() string // or nil
	privateID      PrivateID
	httpDoCalls    atomic.Int32

	procID              uint32
	includeProcSequence bool

	writeLock    sync.Mutex // guards procSequence, flushTimer, buffer.Write calls
	procSequence uint64
	flushTimer   *time.Timer // used when flushDelay is >0

	shutdownStart chan struct{} // closed when shutdown begins
	shutdownDone  chan struct{} // closed when shutdown complete
}

// SetVerbosityLevel controls the verbosity level that should be
// written to stderr. 0 is the default (not verbose). Levels 1 or higher
// are increasingly verbose.
func (l *Logger) SetVerbosityLevel(level int) {
	atomic.StoreInt64(&l.stderrLevel, int64(level))
}

// SetLinkMonitor sets the optional the link monitor.
//
// It should not be changed concurrently with log writes and should
// only be set once.
func (l *Logger) SetLinkMonitor(lm *monitor.Mon) {
	l.linkMonitor = lm
}

// PrivateID returns the logger's private log ID.
//
// It exists for internal use only.
func (l *Logger) PrivateID() PrivateID { return l.privateID }

// Shutdown gracefully shuts down the logger while completing any
// remaining uploads.
//
// It will block, continuing to try and upload unless the passed
// context object interrupts it by being done.
// If the shutdown is interrupted, an error is returned.
func (l *Logger) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			l.uploadCancel()
			<-l.shutdownDone
		case <-l.shutdownDone:
		}
		close(done)
	}()

	close(l.shutdownStart)
	io.WriteString(l, "logger closing down\n")
	<-done

	if l.zstdEncoder != nil {
		return l.zstdEncoder.Close()
	}
	return nil
}

// Close shuts down this logger object, the background log uploader
// process, and any associated goroutines.
//
// Deprecated: use Shutdown
func (l *Logger) Close() {
	l.Shutdown(context.Background())
}

// drainBlock is called by drainPending when there are no logs to drain.
//
// In typical operation, every call to the Write method unblocks and triggers a
// buffer.TryReadline, so logs are written with very low latency.
//
// If the caller specified FlushInterface, drainWake is only sent to
// periodically.
func (l *Logger) drainBlock() (shuttingDown bool) {
	select {
	case <-l.shutdownStart:
		return true
	case <-l.drainWake:
	}
	return false
}

// drainPending drains and encodes a batch of logs from the buffer for upload.
// It uses scratch as its initial buffer.
// If no logs are available, drainPending blocks until logs are available.
func (l *Logger) drainPending(scratch []byte) (res []byte) {
	buf := bytes.NewBuffer(scratch[:0])
	buf.WriteByte('[')
	entries := 0

	var batchDone bool
	const maxLen = 256 << 10
	for buf.Len() < maxLen && !batchDone {
		b, err := l.buffer.TryReadLine()
		if err == io.EOF {
			break
		} else if err != nil {
			b = fmt.Appendf(nil, "reading ringbuffer: %v", err)
			batchDone = true
		} else if b == nil {
			if entries > 0 {
				break
			}

			batchDone = l.drainBlock()
			continue
		}

		if len(b) == 0 {
			continue
		}
		if b[0] != '{' || !json.Valid(b) {
			// This is probably a log added to stderr by filch
			// outside of the logtail logger. Encode it.
			if !l.explainedRaw {
				fmt.Fprintf(l.stderr, "RAW-STDERR: ***\n")
				fmt.Fprintf(l.stderr, "RAW-STDERR: *** Lines prefixed with RAW-STDERR below bypassed logtail and probably come from a previous run of the program\n")
				fmt.Fprintf(l.stderr, "RAW-STDERR: ***\n")
				fmt.Fprintf(l.stderr, "RAW-STDERR:\n")
				l.explainedRaw = true
			}
			fmt.Fprintf(l.stderr, "RAW-STDERR: %s", b)
			// Do not add a client time, as it could have been
			// been written a long time ago. Don't include instance key or ID
			// either, since this came from a different instance.
			b = l.encodeText(b, true, 0, 0, 0)
		}

		if entries > 0 {
			buf.WriteByte(',')
		}
		buf.Write(b)
		entries++
	}

	buf.WriteByte(']')
	if buf.Len() <= len("[]") {
		return nil
	}
	return buf.Bytes()
}

// This is the goroutine that repeatedly uploads logs in the background.
func (l *Logger) uploading(ctx context.Context) {
	defer close(l.shutdownDone)

	scratch := make([]byte, 4096) // reusable buffer to write into
	for {
		body := l.drainPending(scratch)
		origlen := -1 // sentinel value: uncompressed
		// Don't attempt to compress tiny bodies; not worth the CPU cycles.
		if l.zstdEncoder != nil && len(body) > 256 {
			zbody := l.zstdEncoder.EncodeAll(body, nil)
			// Only send it compressed if the bandwidth savings are sufficient.
			// Just the extra headers associated with enabling compression
			// are 50 bytes by themselves.
			if len(body)-len(zbody) > 64 {
				origlen = len(body)
				body = zbody
			}
		}

		for len(body) > 0 {
			select {
			case <-ctx.Done():
				return
			default:
			}
			uploaded, err := l.upload(ctx, body, origlen)
			if err != nil {
				if !l.internetUp() {
					fmt.Fprintf(l.stderr, "logtail: internet down; waiting\n")
					l.awaitInternetUp(ctx)
					continue
				}
				fmt.Fprintf(l.stderr, "logtail: upload: %v\n", err)
			}
			l.bo.BackOff(ctx, err)
			if uploaded {
				break
			}
		}

		select {
		case <-l.shutdownStart:
			return
		default:
		}
	}
}

func (l *Logger) internetUp() bool {
	if l.linkMonitor == nil {
		// No way to tell, so assume it is.
		return true
	}
	return l.linkMonitor.InterfaceState().AnyInterfaceUp()
}

func (l *Logger) awaitInternetUp(ctx context.Context) {
	upc := make(chan bool, 1)
	defer l.linkMonitor.RegisterChangeCallback(func(changed bool, st *interfaces.State) {
		if st.AnyInterfaceUp() {
			select {
			case upc <- true:
			default:
			}
		}
	})()
	if l.internetUp() {
		return
	}
	select {
	case <-upc:
		fmt.Fprintf(l.stderr, "logtail: internet back up\n")
	case <-ctx.Done():
	}
}

// upload uploads body to the log server.
// origlen indicates the pre-compression body length.
// origlen of -1 indicates that the body is not compressed.
func (l *Logger) upload(ctx context.Context, body []byte, origlen int) (uploaded bool, err error) {
	const maxUploadTime = 45 * time.Second
	ctx, cancel := context.WithTimeout(ctx, maxUploadTime)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", l.url, bytes.NewReader(body))
	if err != nil {
		// I know of no conditions under which this could fail.
		// Report it very loudly.
		// TODO record logs to disk
		panic("logtail: cannot build http request: " + err.Error())
	}
	if origlen != -1 {
		req.Header.Add("Content-Encoding", "zstd")
		req.Header.Add("Orig-Content-Length", strconv.Itoa(origlen))
	}
	req.Header["User-Agent"] = nil // not worth writing one; save some bytes

	compressedNote := "not-compressed"
	if origlen != -1 {
		compressedNote = "compressed"
	}

	l.httpDoCalls.Add(1)
	resp, err := l.httpc.Do(req)
	if err != nil {
		return false, fmt.Errorf("log upload of %d bytes %s failed: %v", len(body), compressedNote, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		uploaded = resp.StatusCode == 400 // the server saved the logs anyway
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return uploaded, fmt.Errorf("log upload of %d bytes %s failed %d: %q", len(body), compressedNote, resp.StatusCode, b)
	}

	// Try to read to EOF, in case server's response is
	// chunked. We want to reuse the TCP connection if it's
	// HTTP/1. On success, we expect 0 bytes.
	// TODO(bradfitz): can remove a few days after 2020-04-04 once
	// server is fixed.
	if resp.ContentLength == -1 {
		resp.Body.Read(make([]byte, 1))
	}
	return true, nil
}

// Flush uploads all logs to the server.
// It blocks until complete or there is an unrecoverable error.
func (l *Logger) Flush() error {
	return nil
}

// logtailDisabled is whether logtail uploads to logcatcher are disabled.
var logtailDisabled atomic.Bool

// Disable disables logtail uploads for the lifetime of the process.
func Disable() {
	logtailDisabled.Store(true)
}

var debugWakesAndUploads = envknob.RegisterBool("TS_DEBUG_LOGTAIL_WAKES")

// tryDrainWake tries to send to lg.drainWake, to cause an uploading wakeup.
// It does not block.
func (l *Logger) tryDrainWake() {
	l.flushPending.Store(false)
	if debugWakesAndUploads() {
		// Using println instead of log.Printf here to avoid recursing back into
		// ourselves.
		println("logtail: try drain wake, numHTTP:", l.httpDoCalls.Load())
	}
	select {
	case l.drainWake <- struct{}{}:
	default:
	}
}

func (l *Logger) sendLocked(jsonBlob []byte) (int, error) {
	if logtailDisabled.Load() {
		return len(jsonBlob), nil
	}

	n, err := l.buffer.Write(jsonBlob)

	if l.flushDelay > 0 {
		if l.flushPending.CompareAndSwap(false, true) {
			if l.flushTimer == nil {
				l.flushTimer = time.AfterFunc(l.flushDelay, l.tryDrainWake)
			} else {
				l.flushTimer.Reset(l.flushDelay)
			}
		}
	} else {
		l.tryDrainWake()
	}
	return n, err
}

// TODO: instead of allocating, this should probably just append
// directly into the output log buffer.
func (l *Logger) encodeText(buf []byte, skipClientTime bool, procID uint32, procSequence uint64, level int) []byte {
	now := l.timeNow()

	// Factor in JSON encoding overhead to try to only do one alloc
	// in the make below (so appends don't resize the buffer).
	overhead := len(`{"text": ""}\n`)
	includeLogtail := !skipClientTime || procID != 0 || procSequence != 0
	if includeLogtail {
		overhead += len(`"logtail": {},`)
	}
	if !skipClientTime {
		overhead += len(`"client_time": "2006-01-02T15:04:05.999999999Z07:00",`)
	}
	if procID != 0 {
		overhead += len(`"proc_id": 4294967296,`)
	}
	if procSequence != 0 {
		overhead += len(`"proc_seq": 9007199254740992,`)
	}
	// TODO: do a pass over buf and count how many backslashes will be needed?
	// For now just factor in a dozen.
	overhead += 12

	// Put a sanity cap on buf's size.
	max := 16 << 10
	if l.lowMem {
		max = 255
	}
	var nTruncated int
	if len(buf) > max {
		nTruncated = len(buf) - max
		// TODO: this can break a UTF-8 character
		// mid-encoding.  We don't tend to log
		// non-ASCII stuff ourselves, but e.g. client
		// names might be.
		buf = buf[:max]
	}

	b := make([]byte, 0, len(buf)+overhead)
	b = append(b, '{')

	if includeLogtail {
		b = append(b, `"logtail": {`...)
		if !skipClientTime {
			b = append(b, `"client_time": "`...)
			b = now.UTC().AppendFormat(b, time.RFC3339Nano)
			b = append(b, `",`...)
		}
		if procID != 0 {
			b = append(b, `"proc_id": `...)
			b = strconv.AppendUint(b, uint64(procID), 10)
			b = append(b, ',')
		}
		if procSequence != 0 {
			b = append(b, `"proc_seq": `...)
			b = strconv.AppendUint(b, procSequence, 10)
			b = append(b, ',')
		}
		b = bytes.TrimRight(b, ",")
		b = append(b, "}, "...)
	}

	if l.metricsDelta != nil {
		if d := l.metricsDelta(); d != "" {
			b = append(b, `"metrics": "`...)
			b = append(b, d...)
			b = append(b, `",`...)
		}
	}

	// Add the log level, if non-zero. Note that we only use log
	// levels 1 and 2 currently. It's unlikely we'll ever make it
	// past 9.
	if level > 0 && level < 10 {
		b = append(b, `"v":`...)
		b = append(b, '0'+byte(level))
		b = append(b, ',')
	}
	b = append(b, "\"text\": \""...)
	for _, c := range buf {
		switch c {
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		default:
			// TODO: what about binary gibberish or non UTF-8?
			b = append(b, c)
		}
	}
	if nTruncated > 0 {
		b = append(b, "…+"...)
		b = strconv.AppendInt(b, int64(nTruncated), 10)
	}
	b = append(b, "\"}\n"...)
	return b
}

func (l *Logger) encodeLocked(buf []byte, level int) []byte {
	if l.includeProcSequence {
		l.procSequence++
	}
	if buf[0] != '{' {
		return l.encodeText(buf, l.skipClientTime, l.procID, l.procSequence, level) // text fast-path
	}

	now := l.timeNow()

	obj := make(map[string]any)
	if err := json.Unmarshal(buf, &obj); err != nil {
		for k := range obj {
			delete(obj, k)
		}
		obj["text"] = string(buf)
	}
	if txt, isStr := obj["text"].(string); l.lowMem && isStr && len(txt) > 254 {
		// TODO(crawshaw): trim to unicode code point
		obj["text"] = txt[:254] + "…"
	}

	hasLogtail := obj["logtail"] != nil
	if hasLogtail {
		obj["error_has_logtail"] = obj["logtail"]
		obj["logtail"] = nil
	}
	if !l.skipClientTime || l.procID != 0 || l.procSequence != 0 {
		logtail := map[string]any{}
		if !l.skipClientTime {
			logtail["client_time"] = now.UTC().Format(time.RFC3339Nano)
		}
		if l.procID != 0 {
			logtail["proc_id"] = l.procID
		}
		if l.procSequence != 0 {
			logtail["proc_seq"] = l.procSequence
		}
		obj["logtail"] = logtail
	}
	if level > 0 {
		obj["v"] = level
	}

	b, err := json.Marshal(obj)
	if err != nil {
		fmt.Fprintf(l.stderr, "logtail: re-encoding JSON failed: %v\n", err)
		// I know of no conditions under which this could fail.
		// Report it very loudly.
		panic("logtail: re-encoding JSON failed: " + err.Error())
	}
	b = append(b, '\n')
	return b
}

// Logf logs to l using the provided fmt-style format and optional arguments.
func (l *Logger) Logf(format string, args ...any) {
	fmt.Fprintf(l, format, args...)
}

// Write logs an encoded JSON blob.
//
// If the []byte passed to Write is not an encoded JSON blob,
// then contents is fit into a JSON blob and written.
//
// This is intended as an interface for the stdlib "log" package.
func (l *Logger) Write(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	level, buf := parseAndRemoveLogLevel(buf)
	if l.stderr != nil && l.stderr != io.Discard && int64(level) <= atomic.LoadInt64(&l.stderrLevel) {
		if buf[len(buf)-1] == '\n' {
			l.stderr.Write(buf)
		} else {
			// The log package always line-terminates logs,
			// so this is an uncommon path.
			withNL := append(buf[:len(buf):len(buf)], '\n')
			l.stderr.Write(withNL)
		}
	}

	l.writeLock.Lock()
	defer l.writeLock.Unlock()

	b := l.encodeLocked(buf, level)
	_, err := l.sendLocked(b)
	return len(buf), err
}

var (
	openBracketV = []byte("[v")
	v1           = []byte("[v1] ")
	v2           = []byte("[v2] ")
	vJSON        = []byte("[v\x00JSON]") // precedes log level '0'-'9' byte, then JSON value
)

// level 0 is normal (or unknown) level; 1+ are increasingly verbose
func parseAndRemoveLogLevel(buf []byte) (level int, cleanBuf []byte) {
	if len(buf) == 0 || buf[0] == '{' || !bytes.Contains(buf, openBracketV) {
		return 0, buf
	}
	if bytes.Contains(buf, v1) {
		return 1, bytes.ReplaceAll(buf, v1, nil)
	}
	if bytes.Contains(buf, v2) {
		return 2, bytes.ReplaceAll(buf, v2, nil)
	}
	if i := bytes.Index(buf, vJSON); i != -1 {
		rest := buf[i+len(vJSON):]
		if len(rest) >= 2 {
			v := rest[0]
			if v >= '0' && v <= '9' {
				return int(v - '0'), rest[1:]
			}
		}
	}
	return 0, buf
}
