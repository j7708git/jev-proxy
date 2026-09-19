package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"jev-proxy/internal/calibrate"
	"jev-proxy/internal/rubric"
)

// calibrateMain implements `jev-proxy calibrate --set ./tests --rubric v1`:
// replay the labeled case set through the production scoring pipeline and
// report the five acceptance gates from the plan doc. Exit code is non-zero
// when any gate fails, so the harness can gate rubric version finalization.
func calibrateMain(args []string) {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	set := fs.String("set", "tests", "calibration set: a .jsonl file or directory of .jsonl files")
	ver := fs.String("rubric", "", "rubric version the set was authored against (must match the compiled-in one)")
	jsonOut := fs.String("json", "", "also write full per-case results as JSON to this path")
	cfg := boot(fs, args)

	if *ver != "" && *ver != rubric.Version {
		log.Fatalf("jev-proxy calibrate: set is for rubric %q but this binary carries %q — rebuild or check out the matching version", *ver, rubric.Version)
	}
	cases, err := calibrate.LoadSet(*set)
	if err != nil {
		log.Fatalf("jev-proxy calibrate: %v", err)
	}
	log.Printf("jev-proxy calibrate: replaying %d cases via %s (rubric %s)", len(cases), cfg.Jev.Provider, rubric.Version)

	start := time.Now()
	outs := calibrate.Run(context.Background(), newJevClient(cfg), cases, &cfg.Scoring)
	rep := calibrate.Evaluate(outs, cfg.Scoring.MinConfidence, cfg.Scoring.SafetyFlagAbove, rubric.Version)
	rep.ElapsedMS = time.Since(start).Milliseconds()

	fmt.Print(calibrate.Render(rep))
	if *jsonOut != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err == nil {
			err = os.WriteFile(*jsonOut, b, 0o644)
		}
		if err != nil {
			log.Printf("jev-proxy calibrate: write %s: %v", *jsonOut, err)
		} else {
			log.Printf("jev-proxy calibrate: full results in %s", *jsonOut)
		}
	}
	if !rep.Pass() {
		os.Exit(1)
	}
}
