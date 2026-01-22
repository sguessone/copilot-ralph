package generated

import copilot "github.com/github/copilot-sdk/go"

// Re-export the SDK generated types as aliases so tests that import
// github.com/github/copilot-sdk/go/generated see the same underlying
// types defined in the SDK module.
type SessionEvent = copilot.SessionEvent
type Data = copilot.Data
type Result = copilot.Result
type ErrorUnion = copilot.ErrorUnion
type ErrorClass = copilot.ErrorClass
