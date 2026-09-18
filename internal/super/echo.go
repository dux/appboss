package super

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// Echo mirrors process output to the terminal with a colored "app/proc | " prefix, foreman style.
// Lines from different processes never interleave because every write goes through one mutex.
type Echo struct {
	mu   sync.Mutex
	out  io.Writer
	next int
}

func NewEcho(out io.Writer) *Echo { return &Echo{out: out} }

func (e *Echo) writer(app, proc string) *echoWriter {
	e.mu.Lock()
	defer e.mu.Unlock()
	color := 31 + e.next%6
	e.next++
	return &echoWriter{echo: e, prefix: fmt.Sprintf("\x1b[%dm%s/%s |\x1b[0m ", color, app, proc)}
}

type echoWriter struct {
	echo    *Echo
	prefix  string
	partial []byte
}

func (w *echoWriter) Write(p []byte) (int, error) {
	w.echo.mu.Lock()
	defer w.echo.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		index := bytes.IndexByte(w.partial, '\n')
		if index < 0 {
			return len(p), nil
		}
		w.emit(w.partial[:index+1])
		w.partial = w.partial[index+1:]
	}
}

// flush prints a trailing line that never received its newline, typically the last output before exit.
func (w *echoWriter) flush() {
	w.echo.mu.Lock()
	defer w.echo.mu.Unlock()
	if len(w.partial) > 0 {
		w.emit(append(w.partial, '\n'))
		w.partial = nil
	}
}

func (w *echoWriter) emit(line []byte) {
	_, _ = io.WriteString(w.echo.out, w.prefix)
	_, _ = w.echo.out.Write(line)
}
