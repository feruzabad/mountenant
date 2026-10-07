package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/feruzabad/mountenant/internal/identity/adapters/argon2id"
	"github.com/feruzabad/mountenant/internal/platform/config"
)

// Password length bounds: ASVS 4.0 V2.1.1 (at least 12 characters) and a
// generous maximum so hashing cost stays bounded.
const (
	minPasswordLen = 12
	maxPasswordLen = 1024
)

func hashPassword(e env, args []string) error {
	def := config.Default().Auth.Argon2
	fs := flag.NewFlagSet("hash-password", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	m := fs.Uint("m", uint(def.MemoryKiB), "argon2id memory in KiB")
	t := fs.Uint("t", uint(def.Iterations), "argon2id iterations")
	p := fs.Uint("p", uint(def.Parallelism), "argon2id parallelism")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *m < config.MinArgon2MemoryKiB || *m > 1<<20 || *t < 1 || *t > 64 || *p < 1 || *p > 255 {
		return fmt.Errorf("argon2id parameters out of range (m %d..%d, t 1..64, p 1..255)", config.MinArgon2MemoryKiB, 1<<20)
	}

	pw, err := readPassword(e)
	if err != nil {
		return err
	}
	if n := utf8.RuneCountInString(pw); n < minPasswordLen {
		return fmt.Errorf("password must have at least %d characters", minPasswordLen)
	}
	if len(pw) > maxPasswordLen {
		return fmt.Errorf("password must be at most %d bytes", maxPasswordLen)
	}
	h := argon2id.Hasher{Params: argon2id.Params{MemoryKiB: uint32(*m), Iterations: uint32(*t), Parallelism: uint8(*p)}}
	enc, err := h.Hash(pw)
	if err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, enc)
	return nil
}

// readPassword prompts twice without echo on a terminal, or reads the first
// line of piped input.
func readPassword(e env) (string, error) {
	if f, ok := e.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(e.stderr, "Password: ")
		a, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(e.stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(e.stderr, "Repeat password: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(e.stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("passwords do not match")
		}
		return string(a), nil
	}
	line, err := bufio.NewReader(io.LimitReader(e.stdin, maxPasswordLen+2)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
