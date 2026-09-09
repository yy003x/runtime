//go:build darwin || linux

package terminalinput

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/creack/pty"
)

func terminalFixture(t *testing.T) (*Terminal, *os.File, *os.File) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close(); slave.Close() })
	terminal, err := Open(slave)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminal.Close() })
	return terminal, master, slave
}

func TestTerminalKeysPasteAndRestoration(t *testing.T) {
	terminal, master, slave := terminalFixture(t)
	for _, test := range []struct {
		input string
		key   Key
	}{
		{"/", Key{Text: "/"}}, {"\x1b[A", Key{Name: "up"}}, {"\x1b[B", Key{Name: "down"}},
		{"\t", Key{Name: "tab"}}, {"\r", Key{Name: "enter"}}, {"\x03", Key{Name: "interrupt"}},
		{"中", Key{Text: "中"}}, {"\x1b[200~/logout\n\x1b[B\r\x1b[201~", Key{Text: "/logout [B "}},
	} {
		if _, err := master.Write([]byte(test.input)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		key, err := terminal.ReadKey(ctx)
		cancel()
		if err != nil || key != test.key {
			t.Fatalf("input=%q key=%+v expected=%+v err=%v", test.input, key, test.key, err)
		}
	}
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := getTermios(int(slave.Fd()))
	if err != nil || !reflect.DeepEqual(after, terminal.original) {
		t.Fatal("terminal mode not restored")
	}
}

func TestTerminalPreservesSplitPasteAndUTF8AcrossTimeouts(t *testing.T) {
	terminal, master, _ := terminalFixture(t)
	for _, partial := range []string{"\x1b[20", "0~/logout\n", "\x1b[B\r"} {
		master.Write([]byte(partial))
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
		key, err := terminal.ReadKey(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || key != (Key{}) {
			t.Fatalf("split input executed: %+v %v", key, err)
		}
	}
	master.Write([]byte("\x1b[201~"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	key, err := terminal.ReadKey(ctx)
	if err != nil || key.Name != "" || key.Text != "/logout [B " {
		t.Fatalf("paste=%+v err=%v", key, err)
	}
	master.Write([]byte{0xe4})
	short, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_, err = terminal.ReadKey(short)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	master.Write([]byte{0xb8, 0xad})
	key, err = terminal.ReadKey(ctx)
	if err != nil || key.Text != "中" {
		t.Fatalf("utf8=%+v err=%v", key, err)
	}
}

func TestTerminalEscapeCancellationAndSize(t *testing.T) {
	terminal, master, slave := terminalFixture(t)
	if err := pty.Setsize(slave, &pty.Winsize{Cols: 48, Rows: 12}); err != nil {
		t.Fatal(err)
	}
	width, height := terminal.Size()
	if width != 48 || height != 12 {
		t.Fatal("wrong size")
	}
	master.Write([]byte("\x1b"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	key, err := terminal.ReadKey(ctx)
	cancel()
	if err != nil || key.Name != "escape" {
		t.Fatalf("escape=%+v err=%v", key, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if _, err := terminal.ReadKey(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("read ignored cancellation")
	}
}
