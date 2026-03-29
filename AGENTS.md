# streamloom

streamloom is an in-memory HLS origin server. It accepts segment pushes from a transcoding pipeline and serves HLS streams to viewers. Designed to be lightweight, entirely in-memory, and secure.

## Planning

High level project plans should be stored in `./plans/*.md`.
Once plans are split into tasks, the tasks should be stored in `./plans/TODO.md`.

## Build & Development Commands

- `go build`      - build and check for compiler errors
- `go fix ./...`  - autofix common go errors
- `go fmt ./...`  - apply formatting
- `go vet ./...`  - check for some go errors
- `go test ./...` - run tests
- `gosec ./...`   - run security checks

## Key Guidelines

1. Always run tests, lints, and formatters before committing. All warnings should be treated as errors unless told otherwise.
2. Follow golang best practices and idiomatic patterns.
3. Testing is important for this project. Always ensure that all features are tested and tested well.
4. Avoid adding dependencies. You can add them if needed, but they should have clear justification.
5. Track and update your tasks in `./plans/TODO.md`. Keep tasks small with clear validation criteria.
6. When encountering invalid data, prefer throwing an error over attempting to continue.
7. Never hide errors. Bubble them up and log them as appropriate. If there is no clean recovery, it's better to fail than to continue in an invalid state.
8. Aim for small, focused files. If a file grows too large, consider splitting it into smaller files.
9. When in doubt, ask for help or feedback. It's better to ask questions early than to go down the wrong path for too long.
10. Always keep security in mind.

## Before every commit

Run:
```
go fmt ./...
go fix ./...
go vet ./...
go test ./...
gosec ./...
```
