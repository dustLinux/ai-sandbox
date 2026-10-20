package pool

import cryptorand "crypto/rand"

func randRead(b []byte) (int, error) { return cryptorand.Read(b) }
