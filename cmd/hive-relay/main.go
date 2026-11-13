// Command hive-relay is the former name of ligus-relay, kept so existing
// "go install .../cmd/hive-relay" and service definitions keep working through
// v2.1. It is the same open relay; prefer cmd/ligus-relay.
package main

import "github.com/honeyhive-ai/relay"

func main() { relay.Main() }
