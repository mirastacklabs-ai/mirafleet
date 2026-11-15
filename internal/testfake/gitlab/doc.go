// Package gitlab is a test double for a corporate GitLab, used by the M7 integration tests
// (build tag linux && integration). It serves git over HTTPS (git http-backend through
// net/http/cgi) and over SSH (x/crypto/ssh running git upload-pack / receive-pack), plus a
// minimal OCI registry with a bearer-token exchange, all with certificates signed by a
// throwaway "corporate" CA. Every credential is generated at test time and only one deploy key
// and one project token are accepted. Test-only: never built into the mirafleet binary.
package gitlab
