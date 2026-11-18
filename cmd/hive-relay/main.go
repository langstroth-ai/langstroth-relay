// Command hive-relay is the former name of langstroth-relay, kept so existing
// "go install .../cmd/hive-relay" and service definitions keep working until
// Ligus v3.0. It is the same open relay; prefer cmd/langstroth-relay.
package main

import "github.com/langstroth-ai/langstroth-relay"

func main() { relay.Main() }
