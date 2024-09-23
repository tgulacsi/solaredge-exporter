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

	grp, ctx := errgroup.WithContext(ctx)
	grp.Go(func() error {
		ticker := time.NewTicker(15 * time.Minute)
		var labels []string
		var mGridCP, mLoadCP, mPVCP, mStorageCP, mStorageLevel *metrics.Gauge
		for {
			labels = labels[:0]
			for _, site := range sites {
				nm := metricName{SiteID: site.ID}
				if flow, err := site.GetPowerFlow(ctx); err != nil {
					logger.Error("GetPowerFlow", "error", err)
				} else {
					if mGridCP == nil {
						nm := nm
						nm.Name = "current_power"
						nm.Flow = "grid"
						mGridCP = metrics.NewGauge(nm.String(), nil)
						nm.Flow = "load"
						mLoadCP = metrics.NewGauge(nm.String(), nil)
						nm.Flow = "PV"
						mPVCP = metrics.NewGauge(nm.String(), nil)
						nm.Flow = "storage"
						mStorageCP = metrics.NewGauge(nm.String(), nil)
						nm.Name = "charge_level"
						mStorageLevel = metrics.NewGauge(nm.String(), nil)
					}
					mGridCP.Set(flow.Grid.CurrentPower)
					mLoadCP.Set(flow.Load.CurrentPower)
					mPVCP.Set(flow.PV.CurrentPower)
					mStorageCP.Set(flow.Storage.CurrentPower)
					mStorageLevel.Set(flow.Storage.ChargeLevel)
				}

				metrics.WritePrometheus(os.Stdout, false)

				inventory, err := site.GetInventory(ctx)
				if err != nil {
					logger.Error("GetInventory", "error", err)
				} else {
					for _, inverter := range inventory.Inverters {
						telemetry, err := inverter.GetTelemetry(ctx, time.Now().Add(-24*time.Hour), time.Now())
						if err != nil {
							logger.Error("GetTelemetry", "inverter", inverter.Name, "error", err)
							continue
						}
						for _, entry := range telemetry {
							fmt.Printf("%s - %s - %5.1f V - %4.1f ºC - %6.1f\n", inverter.Name, entry.Time, entry.DcVoltage, entry.Temperature, entry.TotalActivePower)
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
	Name   string
	Flow   string
	SiteID int
}

func (mn metricName) String() string {
	var buf strings.Builder
	fmt.Fprintf(&buf, `%s{siteID="%d"`, mn.Name, mn.SiteID)
	if mn.Flow != "" {
		fmt.Fprintf(&buf, `,flow=%q`, mn.Flow)
	}
	buf.WriteByte('}')
	return buf.String()
}
