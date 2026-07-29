package tmux

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"
)

func newUUIDv7(now time.Time, source io.Reader) (string, error) {
	if source == nil {
		source = rand.Reader
	}
	var raw [16]byte
	if _, err := io.ReadFull(source, raw[:]); err != nil {
		return "", fmt.Errorf("generate tmux ID: %w", err)
	}
	milliseconds := now.UTC().UnixMilli()
	if milliseconds < 0 || milliseconds > 1<<48-1 {
		return "", fmt.Errorf("tmux ID time is out of range")
	}
	raw[0] = byte(milliseconds >> 40)
	raw[1] = byte(milliseconds >> 32)
	raw[2] = byte(milliseconds >> 24)
	raw[3] = byte(milliseconds >> 16)
	raw[4] = byte(milliseconds >> 8)
	raw[5] = byte(milliseconds)
	raw[6] = raw[6]&0x0f | 0x70
	raw[8] = raw[8]&0x3f | 0x80
	return formatUUID(raw), nil
}

func parseUUIDv7(value string) (time.Time, error) {
	if len(value) != 36 ||
		value[8] != '-' || value[13] != '-' ||
		value[18] != '-' || value[23] != '-' {
		return time.Time{}, fmt.Errorf("invalid tmux_id")
	}
	compact := strings.ReplaceAll(value, "-", "")
	if len(compact) != 32 {
		return time.Time{}, fmt.Errorf("invalid tmux_id")
	}
	var raw [16]byte
	if _, err := hex.Decode(raw[:], []byte(compact)); err != nil {
		return time.Time{}, fmt.Errorf("invalid tmux_id")
	}
	if raw[6]>>4 != 7 || raw[8]>>6 != 2 {
		return time.Time{}, fmt.Errorf("invalid tmux_id")
	}
	var encoded [8]byte
	copy(encoded[2:], raw[:6])
	milliseconds := int64(binary.BigEndian.Uint64(encoded[:]))
	return time.UnixMilli(milliseconds).UTC(), nil
}

func formatUUID(raw [16]byte) string {
	encoded := hex.EncodeToString(raw[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" +
		encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func randomHex(source io.Reader, bytes int) (string, error) {
	if source == nil {
		source = rand.Reader
	}
	value := make([]byte, bytes)
	if _, err := io.ReadFull(source, value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
