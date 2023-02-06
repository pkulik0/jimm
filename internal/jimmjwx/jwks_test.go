package jimmjwx_test

import (
	"context"
	"os"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/google/uuid"
	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"

	"github.com/CanonicalLtd/jimm/internal/jimmjwx"
	"github.com/CanonicalLtd/jimm/internal/jimmtest"
	"github.com/CanonicalLtd/jimm/internal/vault"
)

func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(code)
}

func newStore(t testing.TB) *vault.VaultStore {
	client, path, creds, ok := jimmtest.VaultClient(t, "../../")

	if !ok {
		t.Skip("vault not available")
	}
	return &vault.VaultStore{
		Client:     client,
		AuthSecret: creds,
		AuthPath:   "/auth/approle/login",
		KVPath:     path,
	}
}

func getJWKS(c *qt.C) jwk.Set {
	set, err := jwk.ParseString(`
	{
		"keys": [
		  {
			"alg": "RS256",
			"kty": "RSA",
			"use": "sig",
			"n": "yeNlzlub94YgerT030codqEztjfU_S6X4DbDA_iVKkjAWtYfPHDzz_sPCT1Axz6isZdf3lHpq_gYX4Sz-cbe4rjmigxUxr-FgKHQy3HeCdK6hNq9ASQvMK9LBOpXDNn7mei6RZWom4wo3CMvvsY1w8tjtfLb-yQwJPltHxShZq5-ihC9irpLI9xEBTgG12q5lGIFPhTl_7inA1PFK97LuSLnTJzW0bj096v_TMDg7pOWm_zHtF53qbVsI0e3v5nmdKXdFf9BjIARRfVrbxVxiZHjU6zL6jY5QJdh1QCmENoejj_ytspMmGW7yMRxzUqgxcAqOBpVm0b-_mW3HoBdjQ",
			"e": "AQAB",
			"kid": "32d2b213-d3fe-436c-9d4c-67a673890620"
		  }
		]
	}
	`)
	c.Assert(err, qt.IsNil)
	return set
}

func TestGenerateJWKS(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	jwks, privKeyPem, err := jimmjwx.GenerateJWK(ctx)
	c.Assert(err, qt.IsNil)

	jwksIter := jwks.Keys(ctx)
	jwksIter.Next(ctx)
	key := jwksIter.Pair().Value.(jwk.Key)

	// kid
	_, err = uuid.Parse(key.KeyID())
	c.Assert(err, qt.IsNil)
	// use
	c.Assert(key.KeyUsage(), qt.Equals, "sig")
	// alg
	c.Assert(key.Algorithm(), qt.Equals, jwa.RS256)

	// It's fine for us to just test the key exists.
	c.Assert(string(privKeyPem), qt.Contains, "-----BEGIN RSA PRIVATE KEY-----")
}

// This test is difficult to gauge, as it is truly only time based.
// As such, it will retry 60 times on a 500ms basis.
func TestStartJWKSRotatorWithNoJWKSInTheStore(t *testing.T) {
	c := qt.New(t)
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	store := newStore(c)
	err := store.CleanupJWKS(ctx)
	c.Assert(err, qt.IsNil)

	svc := jimmjwx.NewJWKSService(store)

	tick := make(chan time.Time, 1)
	tick <- time.Now()
	err = svc.StartJWKSRotator(ctx, tick, time.Now().AddDate(0, 3, 0))
	c.Assert(err, qt.IsNil)

	var ks jwk.Set
	// We retry 500ms * 60 (30s)
	for i := 0; i < 60; i++ {
		if ks == nil {
			ks, err = store.GetJWKS(ctx)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if ks != nil {
			break
		}
	}

	c.Assert(err, qt.IsNil)
	key, ok := ks.Key(0)
	c.Assert(ok, qt.IsTrue)
	_, err = uuid.Parse(key.KeyID())
	c.Assert(err, qt.IsNil)
}

// Due to the nature of this test, we do not test exact times (as it will vary drastically machine to machine)
// But rather just ensure the JWKS has infact updated.
//
// So I suppose this test is "best effort", but will only ever pass if the code is truly OK.
func TestStartJWKSRotatorRotatesAJWKS(t *testing.T) {
	c := qt.New(t)
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	store := newStore(c)
	err := store.CleanupJWKS(ctx)
	c.Assert(err, qt.IsNil)

	svc := jimmjwx.NewJWKSService(store)

	// So, we first put a fresh JWKS in the store
	err = store.PutJWKS(ctx, getJWKS(c))
	c.Check(err, qt.IsNil)

	// Get the key we're aware of right now
	ks, err := store.GetJWKS(ctx)
	c.Assert(err, qt.IsNil)
	initialKey, ok := ks.Key(0)
	c.Assert(ok, qt.IsTrue)

	// Start up the rotator
	err = svc.StartJWKSRotator(ctx, time.NewTicker(time.Second).C, time.Now())
	c.Assert(err, qt.IsNil)

	// We retry 500ms * 60 (30s) to test the diff
	for i := 0; i < 60; i++ {
		time.Sleep(500 * time.Millisecond)
		ks2, err := store.GetJWKS(ctx)
		c.Assert(err, qt.IsNil)
		newKey, ok := ks2.Key(0)
		c.Assert(ok, qt.IsTrue)
		if initialKey.KeyID() == newKey.KeyID() {
			break
		}
	}
}
