// Command migrate applies database migrations from ./migrations.
//
// Migrations run as a deliberate step, never automatically on API boot: a
// rolling deploy would otherwise race several instances against the same DDL.
package main

func main() {
	// TODO(scaffold): up / down / status subcommands.
}
