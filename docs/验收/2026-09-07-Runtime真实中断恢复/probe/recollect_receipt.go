// Reproduce collection using an unchanged captured workspace and actual reply.
// No inference is performed, and no existing queue row or output file is changed.
package main

import (
 "encoding/json"
 "fmt"
 "os"

 "github.com/jinyitao123/weave/internal/kernel/engine"
 "github.com/jinyitao123/weave/internal/kernel/runtimes"
)

func main() {
 if len(os.Args) != 4 { panic("usage: recollect_receipt WORKDIR ORIGINAL_RECEIPT DESTINATION") }
 raw, err := os.ReadFile(os.Args[2]); if err != nil { panic(err) }
 var receipt struct { Result runtimes.EngineExecResult `json:"result"` }
 if err := json.Unmarshal(raw, &receipt); err != nil { panic(err) }
 result := engine.RunResult{Status: receipt.Result.Status, Output: receipt.Result.Output}
 // Each registered run started with an empty outputs directory. The input
 // files are the unchanged outputs that actual execution later produced.
 runtimes.CollectRunOutputArtifacts(os.Args[1], runtimes.OutputArtifactSnapshot{}, &result)
 wire, err := json.MarshalIndent(runtimes.CLIEngineExecResult(result), "", "  "); if err != nil { panic(err) }
 if err := os.WriteFile(os.Args[3], append(wire, '\n'), 0600); err != nil { panic(err) }
 fmt.Printf("status=%s artifacts=%d\n", result.Status, len(result.Artifacts))
}
