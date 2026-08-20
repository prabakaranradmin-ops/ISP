//go:build !windows

package winservice

// IsWindowsService always reports false off Windows — there is no Service
// Control Manager to have started this process. Kept as a real function
// rather than a build-tagged-out call site so cmd/api and cmd/radiusd need
// no platform conditionals of their own; they call winservice.IsWindowsService
// unconditionally and it always sends them down the interactive path here.
func IsWindowsService() bool { return false }

// Run is never reached: IsWindowsService() being false means callers never
// take the branch that calls it. It exists only so the package's API is
// identical on every platform and cmd/api / cmd/radiusd compile everywhere
// without a build-tagged file of their own.
func Run(name string, runFn RunFunc) error {
	panic("winservice: Run called with IsWindowsService() false — this is a bug in the caller, not a supported code path")
}

// Fatal is a no-op off Windows: main() already prints the same error to
// stderr on the interactive path, which is the console every non-Windows
// caller has.
func Fatal(name string, err error) {}
