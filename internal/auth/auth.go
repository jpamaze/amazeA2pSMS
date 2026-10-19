// Package auth authenticates customer SMPP binds and HTTP API keys.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrIPNotAllowed       = errors.New("auth: source IP not allowed")
	ErrAccountInactive    = errors.New("auth: account not active")
)

// SMPPCredential is a successful customer bind.
type SMPPCredential struct {
	ID        string
	AccountID string
	SystemID  string
	MaxBinds  int
	TPS       int
}

// Authenticator checks customer credentials against PostgreSQL.
type Authenticator struct {
	pool *pgxpool.Pool

	mu   sync.RWMutex
	keys map[string]cachedKey // API key prefix -> verified key
}

type cachedKey struct {
	hash      string
	accountID string
	expires   time.Time
}

// New returns an Authenticator.
func New(pool *pgxpool.Pool) *Authenticator {
	return &Authenticator{pool: pool, keys: map[string]cachedKey{}}
}

// SMPP checks a bind's system_id and password and the source IP.
func (a *Authenticator) SMPP(ctx context.Context, systemID, password string, remote netip.Addr) (SMPPCredential, error) {
	var (
		c       SMPPCredential
		hash    string
		allowed []netip.Prefix
		status  string
	)
	err := a.pool.QueryRow(ctx, `
		SELECT s.id, s.account_id, s.system_id, s.password_hash, s.allowed_ips, s.max_binds, s.tps_limit, a.status
		FROM smpp_credentials s JOIN accounts a ON a.id = s.account_id
		WHERE s.system_id = $1 AND s.disabled_at IS NULL`, systemID).
		Scan(&c.ID, &c.AccountID, &c.SystemID, &hash, &allowed, &c.MaxBinds, &c.TPS, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		// Spend the same time as a real check so system IDs cannot be probed.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return SMPPCredential{}, ErrInvalidCredentials
	}
	if err != nil {
		return SMPPCredential{}, fmt.Errorf("auth: %w", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return SMPPCredential{}, ErrInvalidCredentials
	}
	if status != "active" {
		return SMPPCredential{}, ErrAccountInactive
	}
	if !ipAllowed(allowed, remote) {
		return SMPPCredential{}, ErrIPNotAllowed
	}
	return c, nil
}

var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy"), bcrypt.DefaultCost)

func ipAllowed(allowed []netip.Prefix, ip netip.Addr) bool {
	if len(allowed) == 0 {
		return true
	}
	ip = ip.Unmap()
	for _, p := range allowed {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// APIKeyPrefix is the fixed start of every key: amz_<prefix>_<secret>.
const APIKeyPrefix = "amz_"

// APIKey checks an HTTP API key and returns its account. Verified keys are
// cached for a minute so the hot path does not hit PostgreSQL.
func (a *Authenticator) APIKey(ctx context.Context, key string) (string, error) {
	prefix, secret, ok := splitKey(key)
	if !ok {
		return "", ErrInvalidCredentials
	}
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])

	a.mu.RLock()
	c, hit := a.keys[prefix]
	a.mu.RUnlock()
	if hit && time.Now().Before(c.expires) {
		if subtle.ConstantTimeCompare([]byte(c.hash), []byte(hash)) == 1 {
			return c.accountID, nil
		}
		return "", ErrInvalidCredentials
	}

	var stored, accountID, status string
	err := a.pool.QueryRow(ctx, `
		SELECT k.key_hash, k.account_id, a.status
		FROM api_keys k JOIN accounts a ON a.id = k.account_id
		WHERE k.prefix = $1 AND k.revoked_at IS NULL`, prefix).Scan(&stored, &accountID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidCredentials
	}
	if err != nil {
		return "", fmt.Errorf("auth: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) != 1 {
		return "", ErrInvalidCredentials
	}
	if status != "active" {
		return "", ErrAccountInactive
	}
	a.mu.Lock()
	a.keys[prefix] = cachedKey{hash: stored, accountID: accountID, expires: time.Now().Add(time.Minute)}
	a.mu.Unlock()
	return accountID, nil
}

func splitKey(key string) (prefix, secret string, ok bool) {
	if !strings.HasPrefix(key, APIKeyPrefix) {
		return "", "", false
	}
	rest := key[len(APIKeyPrefix):]
	i := strings.IndexByte(rest, '_')
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// NewAPIKey generates a key. Only prefix and hash are stored; the full key is
// shown to the customer once.
func NewAPIKey() (full, prefix, hash string, err error) {
	p := make([]byte, 6)
	s := make([]byte, 24)
	if _, err = rand.Read(p); err != nil {
		return
	}
	if _, err = rand.Read(s); err != nil {
		return
	}
	prefix = hex.EncodeToString(p)
	secret := hex.EncodeToString(s)
	sum := sha256.Sum256([]byte(secret))
	return APIKeyPrefix + prefix + "_" + secret, prefix, hex.EncodeToString(sum[:]), nil
}

// HashPassword hashes an SMPP password for storage.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}
