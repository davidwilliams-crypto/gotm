//go:build windows

package ui

import (
	"fmt"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"

	"gotm/internal/metrics"
)

// showAffinityDialog lets the user pick which logical processors a process may use.
func showAffinityDialog(a *App, tg metrics.Target) {
	pid, name := tg.PID, tg.Name
	proc, sys, err := metrics.Affinity(tg)
	if err != nil {
		a.errBox("Processor affinity", err)
		return
	}
	var cpus []int
	for i := 0; i < 64; i++ {
		if sys&(1<<uint(i)) != 0 {
			cpus = append(cpus, i)
		}
	}
	boxes := make([]*walk.CheckBox, len(cpus))
	var items []decl.Widget
	for i, cpu := range cpus {
		i, cpu := i, cpu
		items = append(items, decl.CheckBox{
			AssignTo: &boxes[i], Text: fmt.Sprintf("CPU %d", cpu), Checked: proc&(1<<uint(cpu)) != 0,
		})
	}

	var dlg *walk.Dialog
	var ok, cancel *walk.PushButton
	err = decl.Dialog{
		AssignTo: &dlg, Title: "Processor affinity", DefaultButton: &ok, CancelButton: &cancel,
		MinSize: decl.Size{Width: 420, Height: 200},
		Layout:  decl.VBox{Margins: decl.Margins{Left: 12, Top: 12, Right: 12, Bottom: 12}, Spacing: 8},
		Children: []decl.Widget{
			decl.Label{Text: fmt.Sprintf("Allow “%s” (PID %d) to run on:", name, pid)},
			decl.Composite{Layout: decl.Grid{Columns: 6, Spacing: 6, MarginsZero: true}, Children: items},
			decl.Composite{
				Layout: decl.HBox{MarginsZero: true, Spacing: 8},
				Children: []decl.Widget{
					decl.PushButton{Text: "All", OnClicked: func() {
						for _, b := range boxes {
							b.SetChecked(true)
						}
					}},
					decl.PushButton{Text: "None", OnClicked: func() {
						for _, b := range boxes {
							b.SetChecked(false)
						}
					}},
					decl.HSpacer{},
					decl.PushButton{AssignTo: &ok, Text: "Apply", OnClicked: func() {
						var mask uint64
						for i, b := range boxes {
							if b.Checked() {
								mask |= 1 << uint(cpus[i])
							}
						}
						if mask == 0 {
							walk.MsgBox(dlg, "Processor affinity", "Select at least one processor.", walk.MsgBoxIconWarning)
							return
						}
						if err := metrics.SetAffinity(tg, mask); err != nil {
							walk.MsgBox(dlg, "Processor affinity", err.Error(), walk.MsgBoxIconWarning)
							return
						}
						dlg.Accept()
					}},
					decl.PushButton{AssignTo: &cancel, Text: "Cancel", OnClicked: func() { dlg.Cancel() }},
				},
			},
		},
	}.Create(a.mw)
	if err != nil {
		a.errBox("Processor affinity", err)
		return
	}
	dlg.Run()
}
