// Read-only diagnosis of an actual driver result using the production classifier.
// Build this file alone, like the acceptance driver; it does not execute models.
package main

import (
 "encoding/json"
 "errors"
 "fmt"
 "os"

 "github.com/jinyitao123/weave/internal/base/teamrun"
 "github.com/jinyitao123/weave/internal/kernel/engine"
)

func main() {
 if len(os.Args) != 2 { panic("usage: diagnose_failure DRIVER_RESULT.json") }
 raw, err := os.ReadFile(os.Args[1]); if err != nil { panic(err) }
 var driver struct {
  Case string `json:"case"`
  Error string `json:"error"`
  TaskID string `json:"task_id"`
  Result engine.RunResult `json:"result"`
 }
 if err := json.Unmarshal(raw, &driver); err != nil { panic(err) }
 if driver.Error == "" { panic("actual failed driver result required") }
 classification := teamrun.ClassifyFailure(errors.New(driver.Error))
 output := map[string]any{
  "case": driver.Case, "task_id": driver.TaskID,
  "actual_error": driver.Error, "engine_status": driver.Result.Status,
  "artifacts_returned_to_caller": len(driver.Result.Artifacts),
  "production_failure_class": classification.Class,
  "production_retryable": classification.Retryable,
  "production_reason": classification.Reason,
  "scope": "production classifier applied to actual member result; not proof of Workbench action availability",
 }
 encoded, err := json.MarshalIndent(output, "", "  "); if err != nil { panic(err) }
 fmt.Println(string(encoded))
}
