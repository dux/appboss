package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"dboss/internal/config"

	"golang.org/x/term"
)

// init prints a fully commented starter config for a service (the root dboss.yaml) or an app.
// With no argument it asks which one to generate, defaulting to service.
func (c CLI) init(args []string) error {
	if len(args) > 1 {
		return errors.New("usage: dboss init [service|app]")
	}
	role := ""
	if len(args) == 1 {
		role = templateRole(args[0])
		if role == "" {
			return fmt.Errorf("unknown config type %q (use service or app)", args[0])
		}
	} else {
		selected, err := c.selectTemplateRole()
		if err != nil {
			return err
		}
		role = selected
	}
	template, err := config.Template(role)
	if err != nil {
		return err
	}
	_, err = io.WriteString(c.Out, template)
	return err
}

// askTemplateRole prompts for the config type and defaults to service on an empty answer. It
// reads one line, so `printf '2\n' | dboss init` works without a terminal.
func (c CLI) askTemplateRole() (string, error) {
	fmt.Fprint(c.Err, "Generate config for:\n  1) service (root dboss.yaml)\n  2) app (an app's dboss.yaml)\nSelect [1]: ")
	line, err := bufio.NewReader(c.In).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return config.TemplateService, nil
	}
	role := templateRole(answer)
	if role == "" {
		return "", fmt.Errorf("unknown selection %q (use 1 or 2)", answer)
	}
	return role, nil
}

// selectTemplateRole shows an arrow-key menu on a terminal; when input or output is not a
// terminal it falls back to the typed prompt so scripts and tests still work.
func (c CLI) selectTemplateRole() (string, error) {
	in, ok := c.In.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return c.askTemplateRole()
	}
	out := c.Err
	if file, ok := c.Err.(*os.File); !ok || !term.IsTerminal(int(file.Fd())) {
		file, ok := c.Out.(*os.File)
		if !ok || !term.IsTerminal(int(file.Fd())) {
			return c.askTemplateRole()
		}
		out = file
	}
	labels := []string{"service (root dboss.yaml)", "app (an app's dboss.yaml)"}
	roles := []string{config.TemplateService, config.TemplateApp}
	selected := 0
	draw := func(first bool) {
		if !first {
			fmt.Fprint(out, "\x1b[2A")
		}
		for i, label := range labels {
			marker := "  "
			if i == selected {
				marker = "> "
			}
			fmt.Fprintf(out, "\r\x1b[2K%s%s\n", marker, label)
		}
	}
	fmt.Fprint(out, "Generate config for (up/down, Enter):\n")
	draw(true)
	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return "", err
	}
	defer term.Restore(int(in.Fd()), state)
	for {
		key, err := readKey(in)
		if err != nil {
			return "", err
		}
		switch key {
		case "up":
			if selected > 0 {
				selected--
				draw(false)
			}
		case "down":
			if selected < len(labels)-1 {
				selected++
				draw(false)
			}
		case "1", "2", "enter":
			if key == "1" {
				selected = 0
			}
			if key == "2" {
				selected = 1
			}
			fmt.Fprintf(out, "\x1b[2A\r\x1b[2K%s\n\r\x1b[2K", labels[selected])
			return roles[selected], nil
		case "cancel":
			fmt.Fprint(out, "\x1b[2A\r\x1b[2K\r\x1b[2K")
			return "", errors.New("cancelled")
		}
	}
}

// readKey reads one key in raw mode, translating arrows to up/down and Enter to enter. It
// returns "" for keys it does not use.
func readKey(in *os.File) (string, error) {
	buf := make([]byte, 1)
	if _, err := in.Read(buf); err != nil {
		return "", err
	}
	switch buf[0] {
	case '\r', '\n':
		return "enter", nil
	case 0x03, 'q':
		return "cancel", nil
	case 'k':
		return "up", nil
	case 'j':
		return "down", nil
	case '1':
		return "1", nil
	case '2':
		return "2", nil
	case 0x1b:
		sequence := make([]byte, 2)
		if _, err := io.ReadFull(in, sequence); err != nil {
			return "", err
		}
		if sequence[0] == '[' {
			switch sequence[1] {
			case 'A':
				return "up", nil
			case 'B':
				return "down", nil
			}
		}
	}
	return "", nil
}

// templateRole maps an argument or prompt answer to a template role, "" when unknown.
func templateRole(answer string) string {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "1", "service", "s":
		return config.TemplateService
	case "2", "app", "a":
		return config.TemplateApp
	}
	return ""
}
