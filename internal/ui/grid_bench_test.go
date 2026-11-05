//go:build windows

package ui

import (
	"fmt"
	"math/rand"
	"testing"
)

func benchGridRows(n int, r *rand.Rand) [][]string {
	rows := make([][]string, n)
	for i := range rows {
		pid := fmt.Sprint(4 * (i + 1))
		rows[i] = []string{"TCP", fmt.Sprintf("10.0.0.%d:%d", i%250, 1000+i), fmt.Sprintf("Remote%04d:443", r.Intn(n)), "Established", pid,
			fmt.Sprintf("Program%02d.exe", r.Intn(60)), "Network", "TCP|" + pid}
	}
	return rows
}

func BenchmarkGridUpdate(b *testing.B) {
	for _, sc := range []struct {
		name string
		col  int
	}{{"numeric", 4}, {"text", 5}} {
		b.Run(sc.name+"/2000", func(b *testing.B) {
			r := rand.New(rand.NewSource(1))
			base := benchGridRows(2000, r)
			m := &gridModel{keyCol: 7, sortCol: sc.col, numeric: map[int]bool{4: true}}
			m.set(append([][]string(nil), base...))
			inputs := make([][][]string, 8)
			for k := range inputs {
				in := make([][]string, len(base))
				for i, row := range base {
					c := append([]string(nil), row...)
					c[2] = fmt.Sprintf("Remote%04d:443", r.Intn(9)) // changing values, same identity
					in[i] = c
				}
				r.Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })
				inputs[k] = in
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				in := append([][]string(nil), inputs[i%len(inputs)]...)
				b.StartTimer()
				if !m.update(in) {
					m.set(in)
				}
			}
		})
	}
}
