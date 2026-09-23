package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

// password prints a bcrypt hash for basic_auth. The prompt hides input on a terminal; piped
// input is read as one line so the hash can be scripted.
func (c CLI) password(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: dboss password")
	}
	password, err := c.readPassword("Password: ")
	if err != nil {
		return err
	}
	if len(password) == 0 {
		return errors.New("password is empty")
	}
	if file, ok := c.In.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		confirm, err := c.readPassword("Confirm: ")
		if err != nil {
			return err
		}
		if string(confirm) != string(password) {
			return errors.New("passwords do not match")
		}
	}
	hash, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.Out, string(hash))
	return nil
}

func (c CLI) readPassword(prompt string) ([]byte, error) {
	if file, ok := c.In.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		fmt.Fprint(c.Err, prompt)
		defer fmt.Fprintln(c.Err)
		return term.ReadPassword(int(file.Fd()))
	}
	line, err := bufio.NewReader(c.In).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}
