---
name: run-demo
description: Run the gcdeadtrace demo with a specified mode and duration, save output to the output/ directory, and print a verification summary. Invoke when the user says "run demo", "run gcdeadtrace demo", "/run-demo", or asks about demo output.
allowed-tools: [Bash, Read, Grep]
---

# gcdeadtrace Demo Runner

Runs `会话内存/demo/main.go` with GODEBUG=gcdeadtrace=1, saves stdout/stderr
to the output/ directory, and prints a verification summary.

## Usage

```
/run-demo              # all modes, 10 seconds
/run-demo reuse        # reuse mode only
/run-demo session 5s   # session mode, 5 seconds
```

## Arguments

| Position | Name     | Default | Description |
|----------|----------|---------|-------------|
| 1        | mode     | all     | all, reuse, session, concurrent, loop, mixed, fullydead, customtypes, reprint, concurrentgrowth, sessionrefoverflow, largeoutput |
| 2        | duration | 10s     | Run duration (e.g. 5s, 30s, 2m) |

## Output Files

- `会话内存/demo/output/{mode}_stdout.log` — demo program stdout
- `会话内存/demo/output/{mode}_stderr.log` — gcdeadtrace diagnostic output

## Verification

After running, the skill prints PASS/FAIL checks:
- **reuse mode**: gen 0/1/2+ started + per-session lines with `#1`/`#2` suffix
- **session mode**: session breakdown output present
- **concurrentgrowth mode**: session 6001 output + freed/alive sections
- **sessionrefoverflow mode**: session 7000-7024 output + freed/alive sections
- **largeoutput mode**: session 8000-8099 output + freed/alive sections
- **other modes**: any GC output and session breakdown present
