// Package ids generates prefixed, time-sortable, URL-safe identifiers.
package ids

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"strings"
	"time"
)

var enc = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// New returns prefix_<time-sortable random id>, e.g. "msg_01j9zk...".
func New(prefix string) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixMilli()))
	_, _ = rand.Read(b[8:])
	return prefix + "_" + strings.ToLower(enc.EncodeToString(b[:]))
}
