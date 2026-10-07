package argon2id

import (
	"errors"
	"strings"
	"testing"

	"github.com/feruzabad/mountenant/internal/identity/domain"
)

// cheap keeps the tests fast; production uses RFC9106.
var cheap = Hasher{Params: Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1}}

func TestHashAndVerify(t *testing.T) {
	enc, err := cheap.Hash("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("encoding %s", enc)
	}
	if ok, err := cheap.Verify("correct horse", enc); !ok || err != nil {
		t.Fatalf("verify: %v %v", ok, err)
	}
	if ok, _ := cheap.Verify("correct horsE", enc); ok {
		t.Fatal("wrong password accepted")
	}
	other, _ := cheap.Hash("correct horse")
	if other == enc {
		t.Fatal("salt not random")
	}
}

// TestKnownVector checks interoperability with other argon2id
// implementations. The vector comes from argon2-cffi (the Python binding of
// the reference C implementation):
// hash_secret(b"password", b"somesaltsomesalt", time_cost=2,
// memory_cost=65536, parallelism=4, hash_len=32, type=Type.ID).
func TestKnownVector(t *testing.T) {
	enc := "$argon2id$v=19$m=65536,t=2,p=4$c29tZXNhbHRzb21lc2FsdA$72jmXzYpv/28yBx0iMOh0ZS3aKMtsaKFdaTWddug2g8"
	ok, err := Hasher{}.Verify("password", enc)
	if err != nil || !ok {
		t.Fatalf("reference vector: %v %v", ok, err)
	}
}

func TestVerifyUsesParamsFromHash(t *testing.T) {
	enc, _ := cheap.Hash("pw")
	stronger := Hasher{Params: Params{MemoryKiB: 128, Iterations: 2, Parallelism: 2}}
	if ok, _ := stronger.Verify("pw", enc); !ok {
		t.Fatal("old hash no longer verifies after a parameter change")
	}
}

func TestMalformed(t *testing.T) {
	for _, enc := range []string{
		"",
		"hunter2",
		"$argon2i$v=19$m=64,t=1,p=1$c29tZXNhbHQ$CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc",
		"$argon2id$v=16$m=64,t=1,p=1$c29tZXNhbHQ$CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc",
		"$argon2id$v=19$m=99999999,t=1,p=1$c29tZXNhbHQ$CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc",
		"$argon2id$v=19$m=64,t=0,p=1$c29tZXNhbHQ$CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc",
		"$argon2id$v=19$m=64,t=1,p=1,x=2$c29tZXNhbHQ$CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc",
		"$argon2id$v=19$m=64,t=1,p=1$!!$CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc",
		"$argon2id$v=19$m=64,t=1,p=1$c29tZXNhbHQ$c2hvcnQ",
	} {
		ok, err := cheap.Verify("pw", enc)
		if ok || !errors.Is(err, domain.ErrMalformedHash) {
			t.Errorf("%q: ok=%v err=%v", enc, ok, err)
		}
	}
}
