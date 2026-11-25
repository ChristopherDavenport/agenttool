package mcpserver

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The Standard Webhooks secret a client supplies: "whsec_" and the
// base64 of 24 to 64 random bytes.
const (
	webhookSecretPrefix = "whsec_"
	minSecretBytes      = 24
	maxSecretBytes      = 64
)

// parseWebhookSecret reads a delivery.secret into the key bytes, or
// says why it is not one. The error never holds the secret.
func parseWebhookSecret(s string) ([]byte, error) {
	enc, ok := strings.CutPrefix(s, webhookSecretPrefix)
	if !ok {
		return nil, errors.New("delivery.secret must start with whsec_")
	}
	key, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(enc)
	}
	if err != nil {
		return nil, errors.New("delivery.secret must be whsec_ and base64")
	}
	if len(key) < minSecretBytes || len(key) > maxSecretBytes {
		return nil, fmt.Errorf("delivery.secret must hold %d to %d bytes, not %d", minSecretBytes, maxSecretBytes, len(key))
	}
	return key, nil
}

// signWebhook is the webhook-signature header for a message: one
// "v1,<base64 HMAC-SHA256(key, id.timestamp.body)>" for each key,
// separated by spaces, which is how a rotation is signed with both
// secrets.
func signWebhook(keys [][]byte, id string, ts int64, body []byte) string {
	prefix := id + "." + strconv.FormatInt(ts, 10) + "."
	sigs := make([]string, 0, len(keys))
	for _, k := range keys {
		m := hmac.New(sha256.New, k)
		m.Write([]byte(prefix))
		m.Write(body)
		sigs = append(sigs, "v1,"+base64.StdEncoding.EncodeToString(m.Sum(nil)))
	}
	return strings.Join(sigs, " ")
}

// messageID makes a webhook-id: "msg_" for an event and "msg_<kind>_"
// for a control envelope, then 16 random bytes in base64url, which is
// letters, digits, "_" and "-" alone. It is never derived from an event
// id.
func messageID(kind string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := base64.RawURLEncoding.EncodeToString(b[:])
	if kind == "" {
		return "msg_" + id
	}
	return "msg_" + kind + "_" + id
}
