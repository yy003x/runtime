//go:build darwin || linux

// Package terminalinput owns cancellable raw terminal I/O, not UI or auth state.
package terminalinput

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type Key struct {
	Name string
	Text string
}

type Terminal struct {
	fd       int
	original *unix.Termios
	sequence string
	escapeAt time.Time
	text     []byte
	pasting  bool
	paste    strings.Builder
	window   string
}

func Open(file *os.File) (*Terminal, error) {
	fd := int(file.Fd())
	original, err := getTermios(fd)
	if err != nil {
		return nil, err
	}
	raw := *original
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 0, 0
	if err := setTermios(fd, &raw); err != nil {
		return nil, err
	}
	return &Terminal{fd: fd, original: original}, nil
}

func (terminal *Terminal) Close() error {
	return setTermios(terminal.fd, terminal.original)
}

func (terminal *Terminal) Size() (int, int) {
	size, err := unix.IoctlGetWinsize(terminal.fd, unix.TIOCGWINSZ)
	if err != nil || size.Col == 0 || size.Row == 0 {
		return 80, 24
	}
	return int(size.Col), int(size.Row)
}

func (terminal *Terminal) readByte(ctx context.Context) (byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		fds := []unix.PollFd{{Fd: int32(terminal.fd), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, 25)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if fds[0].Revents&unix.POLLIN != 0 {
			var data [1]byte
			n, err := unix.Read(terminal.fd, data[:])
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				continue
			}
			if err != nil {
				return 0, err
			}
			if n == 1 {
				return data[0], nil
			}
		}
		if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return 0, io.EOF
		}
	}
}

func (terminal *Terminal) ReadKey(ctx context.Context) (Key, error) {
	if terminal.pasting {
		return terminal.readPaste(ctx)
	}
	if terminal.sequence != "" {
		return terminal.readSequence(ctx)
	}
	if len(terminal.text) != 0 {
		return terminal.readText(ctx)
	}
	b, err := terminal.readByte(ctx)
	if err != nil {
		return Key{}, err
	}
	switch b {
	case 3:
		return Key{Name: "interrupt"}, nil
	case 4:
		return Key{Name: "eof"}, nil
	case 9:
		return Key{Name: "tab"}, nil
	case 10, 13:
		return Key{Name: "enter"}, nil
	case 8, 127:
		return Key{Name: "backspace"}, nil
	case 1:
		return Key{Name: "home"}, nil
	case 5:
		return Key{Name: "end"}, nil
	case 21:
		return Key{Name: "clear"}, nil
	case 27:
		terminal.sequence, terminal.escapeAt = "\x1b", time.Now()
		return terminal.readSequence(ctx)
	}
	terminal.text = []byte{b}
	return terminal.readText(ctx)
}

func (terminal *Terminal) readText(ctx context.Context) (Key, error) {
	for !utf8.FullRune(terminal.text) && len(terminal.text) < utf8.UTFMax {
		next, err := terminal.readByte(ctx)
		if err != nil {
			return Key{}, err
		}
		terminal.text = append(terminal.text, next)
	}
	r, _ := utf8.DecodeRune(terminal.text)
	terminal.text = nil
	if r == utf8.RuneError || unicode.IsControl(r) {
		return Key{}, nil
	}
	return Key{Text: string(r)}, nil
}

func (terminal *Terminal) readSequence(ctx context.Context) (Key, error) {
	if terminal.sequence == "\x1b" {
		escCtx, cancel := context.WithDeadline(ctx, terminal.escapeAt.Add(60*time.Millisecond))
		next, err := terminal.readByte(escCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				terminal.sequence = ""
				return Key{Name: "escape"}, nil
			}
			return Key{}, err
		}
		if next != '[' && next != 'O' {
			terminal.sequence = ""
			return Key{}, nil
		}
		terminal.sequence += string(next)
	}
	for {
		next, err := terminal.readByte(ctx)
		if err != nil {
			return Key{}, err
		}
		// Preserve partial sequences across UI polling deadlines. Bound memory
		// while discarding an unrecognized sequence through its final byte.
		if len(terminal.sequence) < 32 {
			terminal.sequence += string(next)
		}
		if next < 0x40 || next > 0x7e {
			continue
		}
		sequence := strings.TrimPrefix(terminal.sequence, "\x1b")
		terminal.sequence = ""
		if sequence == "[200~" {
			terminal.pasting = true
			return terminal.readPaste(ctx)
		}
		name := map[string]string{"[A": "up", "OA": "up", "[B": "down", "OB": "down", "[C": "right", "[D": "left", "[H": "home", "[F": "end", "[1~": "home", "[4~": "end", "[3~": "delete", "[5~": "pageup", "[6~": "pagedown"}[sequence]
		return Key{Name: name}, nil
	}
}

func (terminal *Terminal) readPaste(ctx context.Context) (Key, error) {
	for {
		next, err := terminal.readByte(ctx)
		if err != nil {
			return Key{}, err
		}
		terminal.window += string(next)
		if strings.HasSuffix(terminal.window, "\x1b[201~") {
			value := terminal.paste.String() + strings.TrimSuffix(terminal.window, "\x1b[201~")
			terminal.pasting, terminal.window = false, ""
			terminal.paste.Reset()
			return Key{Text: strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) {
					return ' '
				}
				if unicode.IsControl(r) || r == utf8.RuneError {
					return -1
				}
				return r
			}, value)}, nil
		}
		if len(terminal.window) > 6 {
			if terminal.paste.Len() < 4096 {
				terminal.paste.WriteByte(terminal.window[0])
			}
			terminal.window = terminal.window[1:]
		}
	}
}
