// Copyright (c) 2013 Uwe Hoffmann. All rights reserved.

/*
Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google Inc. nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
*/

package service

import (
	"bytes"
	"fmt"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/codahale/hdrhistogram"
	"github.com/dustin/go-humanize"
	"github.com/golang/glog"
	"github.com/uwedeportivo/commander"
	"github.com/uwedeportivo/romba/dedup"
	"github.com/uwedeportivo/romba/types"
)

func (rs *RombaService) printVersion(cmd *commander.Command, args []string) error {
	fmt.Fprintf(cmd.Stdout, "romba version %s", Version)
	return nil
}

func (rs *RombaService) deprecated(cmd *commander.Command, args []string) error {
	fmt.Fprintf(cmd.Stdout, "command is deprecated %s", cmd.Name())
	return nil
}

func (rs *RombaService) memstats(cmd *commander.Command, args []string) error {
	rs.jobMutex.Lock()
	defer rs.jobMutex.Unlock()

	debug.FreeOSMemory()

	s := new(runtime.MemStats)
	runtime.ReadMemStats(s)

	fmt.Fprintf(cmd.Stdout, "\n# runtime.MemStats\n")
	fmt.Fprintf(cmd.Stdout, "# Alloc = %s\n", humanize.IBytes(s.Alloc))
	fmt.Fprintf(cmd.Stdout, "# TotalAlloc = %s\n", humanize.IBytes(s.TotalAlloc))
	fmt.Fprintf(cmd.Stdout, "# Sys = %s\n", humanize.IBytes(s.Sys))
	fmt.Fprintf(cmd.Stdout, "# Lookups = %d\n", s.Lookups)
	fmt.Fprintf(cmd.Stdout, "# Mallocs = %d\n", s.Mallocs)
	fmt.Fprintf(cmd.Stdout, "# Frees = %d\n", s.Frees)

	fmt.Fprintf(cmd.Stdout, "# HeapAlloc = %s\n", humanize.IBytes(s.HeapAlloc))
	fmt.Fprintf(cmd.Stdout, "# HeapSys = %s\n", humanize.IBytes(s.HeapSys))
	fmt.Fprintf(cmd.Stdout, "# HeapIdle = %s\n", humanize.IBytes(s.HeapIdle))
	fmt.Fprintf(cmd.Stdout, "# HeapInuse = %s\n", humanize.IBytes(s.HeapInuse))
	fmt.Fprintf(cmd.Stdout, "# HeapReleased = %s\n", humanize.IBytes(s.HeapReleased))
	fmt.Fprintf(cmd.Stdout, "# HeapObjects = %d\n", s.HeapObjects)

	fmt.Fprintf(cmd.Stdout, "# Stack = %d / %d\n", s.StackInuse, s.StackSys)
	fmt.Fprintf(cmd.Stdout, "# MSpan = %d / %d\n", s.MSpanInuse, s.MSpanSys)
	fmt.Fprintf(cmd.Stdout, "# MCache = %d / %d\n", s.MCacheInuse, s.MCacheSys)
	fmt.Fprintf(cmd.Stdout, "# BuckHashSys = %d\n", s.BuckHashSys)

	fmt.Fprintf(cmd.Stdout, "# NextGC = %d\n", s.NextGC)
	fmt.Fprintf(cmd.Stdout, "# PauseNs = %d\n", s.PauseNs)
	fmt.Fprintf(cmd.Stdout, "# NumGC = %d\n", s.NumGC)
	fmt.Fprintf(cmd.Stdout, "# EnableGC = %v\n", s.EnableGC)
	fmt.Fprintf(cmd.Stdout, "# DebugGC = %v\n", s.DebugGC)

	return nil
}

func (rs *RombaService) dbstats(cmd *commander.Command, args []string) error {
	rs.jobMutex.Lock()
	defer rs.jobMutex.Unlock()

	fmt.Fprintf(cmd.Stdout, "dbstats = %s", rs.romDB.PrintStats())
	return nil
}

type datStats struct {
	h            *hdrhistogram.Histogram
	nRoms        int
	nDats        int
	nGames       int
	totalSize    uint64
	nRomsBelow4k int
}

func (rs *RombaService) datstats(cmd *commander.Command, args []string) error {
	rs.jobMutex.Lock()
	defer rs.jobMutex.Unlock()

	if rs.busy {
		p := rs.pt.GetProgress()

		_, err := fmt.Fprintf(cmd.Stdout, "still busy with %s: (%d of %d files) and (%s of %s) \n", rs.jobName,
			p.FilesSoFar, p.TotalFiles, humanize.IBytes(uint64(p.BytesSoFar)), humanize.IBytes(uint64(p.TotalBytes)))
		return err
	}

	rs.pt.Reset()
	rs.busy = true
	rs.jobName = "datstats"

	go func() {
		glog.Infof("service starting datstats")
		rs.broadCastProgress(time.Now(), true, false, "", nil)
		ticker := time.NewTicker(time.Second * 5)
		stopTicker := make(chan bool)
		go func() {
			glog.Infof("starting progress broadcaster")
			for {
				select {
				case t := <-ticker.C:
					rs.broadCastProgress(t, false, false, "", nil)
				case <-stopTicker:
					glog.Info("stopped progress broadcaster")
					return
				}
			}
		}()

		deduper, err := dedup.NewLevelDBDeduper()
		if err != nil {
			glog.Errorf("error datstats: %v", err)
			rs.broadCastProgress(time.Now(), false, true, "error collecting datstats", err)
			return
		}
		defer deduper.Close()

		dts := &datStats{
			h: hdrhistogram.New(0, 1000000000000, 5),
		}

		err = rs.romDB.ForEachDat(func(dat *types.Dat) error {
			rs.pt.DeclareFile(dat.Name)
			if dat.Generation != rs.romDB.Generation() {
				return nil
			}
			dedat, err := dedup.Dedup(dat, deduper)
			if err != nil {
				return err
			}

			if dedat == nil {
				return nil
			}

			dts.nDats = dts.nDats + 1
			for _, g := range dedat.Games {
				dts.nGames = dts.nGames + 1
				for _, r := range g.Roms {
					dts.h.RecordValue(r.Size)
					dts.nRoms = dts.nRoms + 1
					dts.totalSize = dts.totalSize + uint64(r.Size)
					if r.Size <= 4000 {
						dts.nRomsBelow4k = dts.nRomsBelow4k + 1
					}
				}
			}
			return nil
		})
		if err != nil {
			glog.Errorf("error datstats: %v", err)
			rs.broadCastProgress(time.Now(), false, true, "error collecting datstats", err)
			return
		}

		bs := dts.h.CumulativeDistribution()

		var msgBuffer bytes.Buffer

		fmt.Fprintf(&msgBuffer, "number of dats = %d\n", dts.nDats)
		fmt.Fprintf(&msgBuffer, "number of games = %d\n", dts.nGames)
		fmt.Fprintf(&msgBuffer, "number of roms = %d\n", dts.nRoms)
		fmt.Fprintf(&msgBuffer, "total rom size = %s\n", humanize.IBytes(dts.totalSize))
		fmt.Fprintf(&msgBuffer, "number of roms below 4k size = %d\n\n", dts.nRomsBelow4k)

		fmt.Fprintf(&msgBuffer, "rom size cumulative distribution = \n")
		fmt.Fprintf(&msgBuffer, "count, percentile, file size\n")
		for i := 0; i < len(bs); i++ {
			b := bs[i]

			vstr := humanize.IBytes(uint64(b.ValueAt))

			if (i < len(bs)-1 && vstr != humanize.IBytes(uint64(bs[i+1].ValueAt))) || (i == len(bs)-1) {
				fmt.Fprintf(&msgBuffer, "%d, %.8f, %s\n", b.Count, b.Quantile, humanize.IBytes(uint64(b.ValueAt)))
			}
		}

		fmt.Fprintf(&msgBuffer, "\nrom size histogram = \n")
		fmt.Fprintf(&msgBuffer, "count, file size\n")
		var lastCount int64
		for _, b := range bs {
			count := b.Count - lastCount
			if count > 0 {
				fmt.Fprintf(&msgBuffer, "%d, %s\n", count, humanize.IBytes(uint64(b.ValueAt)))
			}
			lastCount = b.Count
		}

		ticker.Stop()
		stopTicker <- true

		rs.jobMutex.Lock()
		rs.busy = false
		rs.jobName = ""
		rs.jobMutex.Unlock()

		rs.broadCastProgress(time.Now(), false, true, msgBuffer.String(), err)
		glog.Infof("service finished datstats")

	}()

	return nil
}
