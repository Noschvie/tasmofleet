package web

import (
	"bytes"
	"context"
	"html/template"
	"strconv"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The device list renders through the ui components of design v2 (port package F5): the table
// frame (ui.Table) with the rows of deviceRows (cells.templ), and below 45rem the phone cards
// (ui.DeviceCards), whose live parts are the same cells the event stream pushes. Both are rendered for every request; CSS shows one of them, and select.js keeps the
// selection of a device's row and card in step (both boxes carry the device id).

// deviceCols are the header and colgroup classes of the device table, by column id (the
// labels come from the column registry, so the two cannot drift).
var deviceCols = map[string]ui.TableCol{
	"name": {Col: "c-name"}, "status": {Col: "c-status"}, "reach": {Class: "c-reach", Col: "c-reach"}, "firmware": {Col: "c-fw"},
	"family": {Class: "c-fam", Col: "c-fam"}, "address": {Col: "c-addr"}, "uptime": {Class: "c-up", Col: "c-up"},
	"wifi": {Class: "c-wifi", Col: "c-wifi"}, "restart": {Class: "c-restart", Col: "c-restart"},
	"otaurl": {Class: "c-ota", Col: "c-ota"}, "grouptopic": {Class: "c-gt", Col: "c-gt"},
	"power": {Col: "c-pow"}, "actions": {SrOnly: true, Col: "c-act"},
}

// deviceTableData is the frame of the device table for a page.
func deviceTableData(p devicesPage) ui.TableData {
	t := ui.TableData{Key: "devices", Class: "devtable dense", SortBase: p.SortBase, Sort: p.Sort.Key, Dir: p.Sort.Dir}
	if p.RowsData.Select {
		t.Select = "Select all selectable devices"
	}
	for _, cd := range tableDefs["devices"].Cols {
		c := deviceCols[cd.ID]
		c.Label = cd.Label
		t.Cols = append(t.Cols, c)
	}
	return t
}

// rowMenu is the data of a device's row actions (partial "device_menu"), in the table and on a card.
type rowMenu struct {
	Base           string
	Admin, Console bool
	Device         model.Device
	Up             bool // the menu opens upward (last rows; menu.js decides with JavaScript)
}

// Menu is the row actions of the i-th row.
func (d rowsData) Menu(i int) rowMenu {
	return rowMenu{Base: d.Base, Admin: d.Admin, Console: d.Console, Device: d.Rows[i].Device, Up: i >= len(d.Rows)-2}
}

// deviceList renders the table and the phone cards of a device list page into p.
func (h *handler) deviceList(ctx context.Context, p *devicesPage) error {
	if len(p.Rows) == 0 {
		return nil
	}
	var b bytes.Buffer
	if err := ui.Table(deviceTableData(*p)).Render(templ.WithChildren(ctx, h.deviceRows(p.RowsData)), &b); err != nil {
		return err
	}
	p.Table = template.HTML(b.String()) // #nosec G203 -- rendered by templ, which escapes every value
	l := ui.DeviceListData{Select: p.RowsData.Select}
	for i, row := range p.RowsData.Rows {
		id := strconv.FormatInt(int64(row.Device.ID), 10)
		meta := []string{row.Device.Address}
		if row.Device.Port != 0 {
			meta[0] += ":" + strconv.Itoa(row.Device.Port)
		}
		if f := row.Device.Identity.Family; f != model.ChipUnknown && f != "" {
			meta = append(meta, chipLabel(f))
		}
		if v := row.Device.Identity.Version; v != "" {
			meta = append(meta, v)
		}
		l.Cards = append(l.Cards, ui.DeviceCardData{
			ID: id, Name: row.Device.Name, Href: p.RowsData.Base + "/devices/" + id, Meta: meta, Select: p.RowsData.Select,
			Outdated: row.Outdated, Deviations: row.Deviations,
			Status: h.statusCell(row.Health), Stability: stabBadge(row.Stab), Uptime: h.uptimeCell(row.Vitals),
			Wifi: wifiCell(row.Vitals), Power: powerCellView(row.Power),
			Menu: deviceMenu(p.RowsData.Menu(i)),
		})
	}
	b.Reset()
	if err := ui.DeviceCards(l).Render(ctx, &b); err != nil {
		return err
	}
	p.Cards = template.HTML(b.String()) // #nosec G203 -- rendered by templ, which escapes every value
	return nil
}
