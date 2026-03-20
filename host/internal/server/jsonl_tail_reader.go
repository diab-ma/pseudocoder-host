package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"sync"
	"time"
)

// JSONLTailReaderConfig configures a JSONL file tail reader.
type JSONLTailReaderConfig struct {
	// FilePath is the absolute path to the JSONL file to tail.
	FilePath string
	// PollInterval is how often to check for new content. Defaults to 100ms.
	PollInterval time.Duration
	// OnLine is called for each complete, valid JSON line read from the file.
	OnLine func(line []byte)
	// OnReset is called when file truncation or inode change is detected,
	// signaling a provider_reset. The file is re-read from the beginning
	// after this callback returns.
	OnReset func()
	// OnReadComplete is called after the initial read-from-beginning completes
	// (both on initial start and after a provider_reset re-read). This allows
	// consumers to batch-process accumulated items rather than merging per-line.
	OnReadComplete func()
	// OnError is called for non-fatal errors (stat failures, read errors).
	OnError func(error)
}

// JSONLTailReader follows a bound JSONL file, delivering complete JSON lines
// via OnLine and detecting file truncation/rotation via OnReset.
type JSONLTailReader struct {
	config   JSONLTailReaderConfig
	offset   int64       // next byte position to read from in the file
	fileInfo os.FileInfo // last known file info for inode comparison
	buf      []byte      // incomplete trailing line from last read

	stopCh   chan struct{}
	doneCh   chan struct{}
	mu       sync.Mutex
	running  bool
	stopping bool
}

// NewJSONLTailReader creates a new tail reader (not started).
func NewJSONLTailReader(config JSONLTailReaderConfig) *JSONLTailReader {
	if config.PollInterval == 0 {
		config.PollInterval = 100 * time.Millisecond
	}
	return &JSONLTailReader{config: config}
}

// Start begins tailing the file in a background goroutine.
// The entire file is read from the beginning on initial start (full replay).
func (r *JSONLTailReader) Start() {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})
	r.running = true
	r.stopping = false
	stopCh := r.stopCh
	doneCh := r.doneCh
	r.mu.Unlock()

	go r.pollLoop(stopCh, doneCh)
}

// Stop signals the tail reader to exit and waits for it to finish.
func (r *JSONLTailReader) Stop() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	if r.stopping {
		doneCh := r.doneCh
		r.mu.Unlock()
		<-doneCh
		return
	}
	r.stopping = true
	stopCh := r.stopCh
	doneCh := r.doneCh
	r.mu.Unlock()

	close(stopCh)
	<-doneCh

	r.mu.Lock()
	r.running = false
	r.stopping = false
	r.mu.Unlock()
}

// pollLoop reads the file from the beginning, then polls for new content.
func (r *JSONLTailReader) pollLoop(stopCh <-chan struct{}, doneCh chan struct{}) {
	defer close(doneCh)

	// Initial read: full replay from beginning of file.
	r.readFromBeginning()
	if r.config.OnReadComplete != nil {
		r.config.OnReadComplete()
	}

	ticker := time.NewTicker(r.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			r.poll()
		}
	}
}

// readFromBeginning resets state and reads the entire file from offset 0.
// Called on initial bind and after provider_reset.
func (r *JSONLTailReader) readFromBeginning() {
	r.offset = 0
	r.buf = nil

	f, err := os.Open(r.config.FilePath)
	if err != nil {
		r.reportError(err)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		r.reportError(err)
		return
	}
	r.fileInfo = info

	r.readNewContent(f)
}

// poll checks for file changes and reads new content. It opens the file and
// stats the fd (not the path) to avoid TOCTOU races with inode detection.
func (r *JSONLTailReader) poll() {
	f, err := os.Open(r.config.FilePath)
	if err != nil {
		r.reportError(err)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		r.reportError(err)
		return
	}

	// Detect inode change (file replaced/rotated).
	if r.fileInfo != nil && !os.SameFile(r.fileInfo, info) {
		log.Printf("jsonl_tail_reader: inode change detected for %s", r.config.FilePath)
		if r.config.OnReset != nil {
			r.config.OnReset()
		}
		r.readFromBeginning()
		if r.config.OnReadComplete != nil {
			r.config.OnReadComplete()
		}
		return
	}

	// Detect file truncation (size shrinks below our read position).
	if info.Size() < r.offset {
		log.Printf("jsonl_tail_reader: truncation detected for %s (size %d < offset %d)",
			r.config.FilePath, info.Size(), r.offset)
		if r.config.OnReset != nil {
			r.config.OnReset()
		}
		r.readFromBeginning()
		if r.config.OnReadComplete != nil {
			r.config.OnReadComplete()
		}
		return
	}

	// No new content.
	if info.Size() == r.offset {
		return
	}

	// Read new content from current offset.
	r.fileInfo = info
	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		r.reportError(err)
		return
	}

	r.readNewContent(f)
}

// readNewContent reads all available data from f and processes it into lines.
func (r *JSONLTailReader) readNewContent(f *os.File) {
	data, err := io.ReadAll(f)
	if err != nil {
		r.reportError(err)
		return
	}
	r.offset += int64(len(data))

	// Prepend any leftover bytes from a previous incomplete line.
	if len(r.buf) > 0 {
		data = append(r.buf, data...)
		r.buf = nil
	}

	r.deliverLines(data)
}

// deliverLines splits data by newlines and delivers each complete, valid
// JSON line via OnLine. Incomplete trailing data is saved in r.buf.
func (r *JSONLTailReader) deliverLines(data []byte) {
	for len(data) > 0 {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			// Incomplete line — save for next read cycle.
			r.buf = append([]byte(nil), data...)
			return
		}

		line := data[:idx]
		data = data[idx+1:]

		// Skip empty lines.
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		// Skip and log malformed JSON lines (no panic, no stop).
		if !json.Valid(line) {
			log.Printf("jsonl_tail_reader: skipping malformed JSON line: %s",
				jsonlTailTruncateForLog(line))
			continue
		}

		if r.config.OnLine != nil {
			r.config.OnLine(line)
		}
	}
}

func (r *JSONLTailReader) reportError(err error) {
	if r.config.OnError != nil {
		r.config.OnError(err)
	}
}

// jsonlTailTruncateForLog truncates a byte slice to a reasonable length for logging.
func jsonlTailTruncateForLog(data []byte) string {
	const maxLen = 200
	if len(data) <= maxLen {
		return string(data)
	}
	return string(data[:maxLen]) + "..."
}
