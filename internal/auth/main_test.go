package auth

import (
	"os"
	"testing"
)

// testIterations is the PBKDF2 count the tests hash with. At PasswordIterations (600 000) the package
// took 40 s of CPU, and parallel tests do not help: the KDF slots (kdfSlots) cap them (#489).
const testIterations = 1000

// productionIterations is hashIterations as the package initialised it, before TestMain lowered it.
var productionIterations int

func TestMain(m *testing.M) {
	productionIterations = hashIterations
	hashIterations = testIterations
	var err error
	if testHash, err = HashPassword(testPassword); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
