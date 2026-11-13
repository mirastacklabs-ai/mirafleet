package tokens

// Known-answer vectors: secret = 32 x 0x42, host key = ed25519 seed 32 x 0x01.
//
// kat0 and kat1 were cross-checked with OpenSSL/coreutils (not our Go code), run in the pinned
// golang:bookworm image (step 29000000 shown; use 0 for kat0):
//
//	bash -c 'key=$(printf 42%.0s $(seq 32)); s=29000000
//	  (printf mirafleet-register-v1; printf "$(printf "%016x" $s | sed "s/../\\\\x&/g")") |
//	  openssl dgst -sha256 -mac HMAC -macopt hexkey:$key -binary | base32 | tr -d "=" | cut -c1-26'
//
// katHash is checked against ssh.FingerprintSHA256 in TestHostKeyHash.
const (
	kat0    = "USLEMPDJTFTDGJMFGAPEFWUDRG" // step 0
	kat1    = "TDQ3TCYEECYXC3OBEA7N4U4CZK" // step 29000000
	katHash = "fe85JkIjo8VPe-XqXJGH5Mau1EMFdK1OdKvJUFicyA8"
)
