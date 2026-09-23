package supervisor

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// Echo mirrors process output to the terminal with a colored "app/proc | " prefix, foreman style.
// Lines from different processes never interleave because every write goes through one mutex.
// Every name is padded to the widest one seen, so the pipes line up in one column.
type Echo struct {
	mu     sync.Mutex
	out    io.Writer
	next   int
	colors map[string]int
	width  int
	soloed bool
}

func NewEcho(out io.Writer) *Echo { return &Echo{out: out} }

// Solo drops the app segment from every key. Running `dboss start` inside an app folder serves
// exactly one app, so repeating its name on every line is noise: `job |` says as much as
// `sinatra/job |` when there is no other app to confuse it with.
func (e *Echo) Solo() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.soloed = true
}

func (e *Echo) name(app, proc string) string {
	if e.soloed {
		return proc
	}
	return app + "/" + proc
}

// Key is the colored "app/proc |" prefix for one process. Its color is assigned on first ask and
// kept for the session, so the startup banner row and the process's later log lines share it and
// a restart keeps it. The name is padded to the widest name registered so far.
func (e *Echo) Key(app, proc string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.key(e.name(app, proc))
}

func (e *Echo) key(name string) string {
	color, ok := e.colors[name]
	if !ok {
		if e.colors == nil {
			e.colors = map[string]int{}
		}
		color = 31 + e.next%6
		e.next++
		e.colors[name] = color
		e.width = max(e.width, len(name))
	}
	return fmt.Sprintf("\x1b[%dm%-*s |\x1b[0m ", color, e.width, name)
}

// Print writes one already-keyed line through the same lock the process writers use, so a
// banner row can never land in the middle of a log line.
func (e *Echo) Print(line string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = io.WriteString(e.out, line+"\n")
}

func (e *Echo) writer(app, proc string) *echoWriter {
	e.mu.Lock()
	defer e.mu.Unlock()
	name := e.name(app, proc)
	e.key(name)
	return &echoWriter{echo: e, name: name}
}

type echoWriter struct {
	echo    *Echo
	name    string
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
	// Rendered per line, so a process named before a wider one still lines up afterwards.
	_, _ = io.WriteString(w.echo.out, w.echo.key(w.name))
	_, _ = w.echo.out.Write(line)
}
