// Copyright 2024 Tamás Gulácsi. All rights reserved.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/UNO-SOFT/zlog/v2"
	"github.com/VictoriaMetrics/metrics"
	"github.com/clambin/solaredge"
	"github.com/tgulacsi/go/httpunix"
	"golang.org/x/sync/errgroup"
)

func main() {
	if err := Main(); err != nil {
		logger.Error("MAIN", "error", err)
		os.Exit(1)
	}
}

var verbose = zlog.VerboseVar(1)
var logger = slog.New(zlog.MaybeConsoleHandler(&verbose, os.Stderr))

func Main() error {
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	c := solaredge.Client{Token: os.Getenv("SOLAREDGE_API_KEY")}
	sites, err := c.GetSites(ctx)
	if err != nil {
		return err
	}

	type flowMetrics struct {
		GridCP, LoadCP, PVCP, StorageCP, StorageLevel *metrics.Gauge
	}
	type inverterMetric struct {
		Voltage, Temperature, TotalActivePower, TotalEnergy *metrics.Gauge
	}
	type siteMetrics struct {
		SiteID    int
		Flow      flowMetrics
		Inverters map[string]inverterMetric
	}
	grp, ctx := errgroup.WithContext(ctx)
	grp.Go(func() error {
		ticker := time.NewTicker(15 * time.Minute)
		ms := make(map[int]siteMetrics, 1)
		for {
			for _, site := range sites {
				s, ok := ms[site.ID]
				if !ok {
					G := func(f, m string) *metrics.Gauge {
						return metrics.NewGauge(metricName{
							Metric: m, Value: f,
							Name: "flow", SiteID: site.ID,
						}.String(), nil)
					}
					s = siteMetrics{
						SiteID:    site.ID,
						Inverters: make(map[string]inverterMetric, 1),
						Flow: flowMetrics{
							GridCP:       G("grid", "current_power"),
							LoadCP:       G("load", "current_power"),
							PVCP:         G("PV", "current_power"),
							StorageCP:    G("storage", "current_power"),
							StorageLevel: G("storage", "current_level"),
						},
					}
					ms[site.ID] = s
				}
				if flow, err := site.GetPowerFlow(ctx); err != nil {
					logger.Error("GetPowerFlow", "error", err)
				} else {
					s.Flow.GridCP.Set(flow.Grid.CurrentPower)
					s.Flow.LoadCP.Set(flow.Load.CurrentPower)
					s.Flow.PVCP.Set(flow.PV.CurrentPower)
					s.Flow.StorageCP.Set(flow.Storage.CurrentPower)
					s.Flow.StorageLevel.Set(flow.Storage.ChargeLevel)
				}

				metrics.WritePrometheus(os.Stdout, false)

				inverters, err := site.GetInverters(ctx)
				if err != nil {
					logger.Error("GetInverters", "error", err)
				} else {
					logger.Info("inverters", "inverters", inverters)
					for _, inverter := range inverters {
						inv, ok := s.Inverters[inverter.SerialNumber]
						if !ok {
							G := func(m string) *metrics.Gauge {
								return metrics.NewGauge(metricName{
									Metric: m, Name: "inverter",
									Value: inverter.SerialNumber, SiteID: site.ID,
								}.String(), nil)
							}
							inv.Temperature = G("temperature")
							inv.TotalActivePower = G("total_active_power")
							inv.TotalEnergy = G("total_energy")
							inv.Voltage = G("voltage")
							s.Inverters[inverter.SerialNumber] = inv
						}
						telemetry, err := inverter.GetTelemetry(ctx, time.Now().Add(-29*time.Minute), time.Now())
						if err != nil {
							logger.Error("GetTelemetry", "inverter", inverter.Name, "error", err)
							continue
						}
						for _, entry := range telemetry {
							inv.Voltage.Set(entry.DcVoltage)
							inv.Temperature.Set(entry.Temperature)
							inv.TotalActivePower.Set(entry.TotalActivePower)
							inv.TotalEnergy.Set(entry.TotalEnergy)
						}
					}
				}
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return nil
			}
		}
	})

	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		metrics.WritePrometheus(w, true)
	})
	grp.Go(func() error { return httpunix.ListenAndServe(ctx, flag.Arg(0), http.DefaultServeMux) })
	return grp.Wait()
}

type metricName struct {
	Metric      string
	Name, Value string
	SiteID      int
}

func (mn metricName) String() string {
	var buf strings.Builder
	fmt.Fprintf(&buf, `%s{siteID="%d",%s=%q}`, mn.Metric, mn.SiteID, mn.Name, mn.Value)
	return buf.String()
}
