package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

type probe struct {
	Path    string         `json:"path"`
	Status  int            `json:"status"`
	Body    map[string]any `json:"body,omitempty"`
	Problem string         `json:"error,omitempty"`
}

// probes is shared by tokenmeter health and the MCP health tool.
func (a *app) probes(ctx context.Context) (base string, probes []probe, ready bool, err error) {
	base, err = a.t.httpBase()
	if err != nil {
		return "", nil, false, err
	}
	ready = true
	for _, path := range []string{"/healthz", "/readyz"} {
		pr := probe{Path: path}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return "", nil, false, err
		}
		res, err := a.t.httpClient().Do(req)
		if err != nil {
			pr.Problem, ready = err.Error(), false
		} else {
			raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
			res.Body.Close()
			pr.Status = res.StatusCode
			_ = json.Unmarshal(raw, &pr.Body)
			ready = ready && res.StatusCode == http.StatusOK
		}
		probes = append(probes, pr)
	}
	return base, probes, ready, nil
}

func (a *app) healthCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check /healthz and /readyz and show the service's version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			ctx, cancel := a.ctx()
			defer cancel()
			base, probes, ready, err := a.probes(ctx)
			if err != nil {
				return err
			}
			if p.json() {
				if err := p.data(map[string]any{"http": base, "probes": probes}); err != nil {
					return err
				}
			} else {
				var rows [][]string
				for _, pr := range probes {
					st, detail := strings.TrimSpace(fmt.Sprint(pr.Status)), pr.Problem
					if pr.Problem == "" {
						detail = fmt.Sprint(pr.Body["status"])
						if v, ok := pr.Body["version"]; ok {
							detail += ", version " + fmt.Sprint(v)
						}
						if d, ok := pr.Body["database"]; ok {
							detail += ", database " + fmt.Sprint(d)
						}
					} else {
						st = "-"
					}
					rows = append(rows, []string{base + pr.Path, st, detail})
				}
				if err := p.rows([]string{"PROBE", "STATUS", "DETAIL"}, rows); err != nil {
					return err
				}
			}
			if !ready {
				return codeError{exitRetry}
			}
			return nil
		},
	}
	outputFlag(cmd, &format)
	return cmd
}
