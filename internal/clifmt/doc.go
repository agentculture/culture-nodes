// Package clifmt implements the agent-first CLI output/error/exit contract
// shared across the AgentCulture repo family (devague, headspace-cli,
// ec2bedrock-cli, and this repo's own Python culture_nodes/cli). It is a
// port of the CONTRACT those repos share, not a transliteration of any one
// implementation:
//
//   - results go to stdout, errors and diagnostics go to stderr, and the
//     two streams are never mixed;
//   - failures are a structured CliError{Code, Message, Remediation}: text
//     mode renders "error: <message>" then "hint: <remediation>"; JSON mode
//     renders {"code","message","remediation"} as single-line JSON;
//   - the exit-code policy is 0 success, 1 user-input error, 2
//     environment/setup error, 3+ reserved;
//   - --json must work even for parse-time failures (a bad verb or a bad
//     flag before any command-specific parsing happens), so callers
//     pre-scan argv for --json with HasJSONFlag/StripJSONFlag instead of
//     discovering it only after a flag.FlagSet successfully parses;
//   - no panic or unexpected error may ever reach the user as a stack
//     trace — Guard recovers/normalises anything unexpected into a
//     CliError in the environment-error bucket.
//
// cmd/nodes wires this contract onto the actual verb surface (whoami,
// learn, explain, overview, doctor, cli overview, and the recognized-but-
// not-yet-implemented process modes).
package clifmt
