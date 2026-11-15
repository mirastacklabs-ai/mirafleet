package central

import "github.com/mirastacklabs-ai/mirafleet/internal/protover"

// testProto is the protocol level raw test heartbeats claim: heartbeats without one are refused
// (docs/M6.md 2.2).
const testProto = protover.Current
