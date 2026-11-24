package mdtable

import "testing"

func check(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBasic(t *testing.T) {
	check(t, Table([]string{"Name", "Qty"}, [][]string{{"pen", "2"}, {"notebook", "10"}}), ""+
		"| Name     | Qty |\n"+
		"| -------- | --- |\n"+
		"| pen      | 2   |\n"+
		"| notebook | 10  |\n")
}

func TestCountsCharacters(t *testing.T) {
	check(t, Table([]string{"City"}, [][]string{{"Zürich"}, {"São Paulo"}}), ""+
		"| City      |\n"+
		"| --------- |\n"+
		"| Zürich    |\n"+
		"| São Paulo |\n")
}

func TestShortRowsAndMinimumWidth(t *testing.T) {
	check(t, Table([]string{"ID", "Note"}, [][]string{{"1"}, {"2", "ok"}}), ""+
		"| ID  | Note |\n"+
		"| --- | ---- |\n"+
		"| 1   |      |\n"+
		"| 2   | ok   |\n")
}

func TestEscapesPipes(t *testing.T) {
	check(t, Table([]string{"Expr"}, [][]string{{"a|b"}}), ""+
		"| Expr |\n"+
		"| ---- |\n"+
		"| a\\|b |\n")
}
