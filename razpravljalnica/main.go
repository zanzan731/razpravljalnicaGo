package main

import "razpravljalnica/cmd"

//glavna datoteka ta datoteka basicly pove kje je moj entry point,
//  zato lahko zaganjam tako pogleda pole v cmd in pogleda Root kjer je execute in pole znotraj datoteke ki jo klicem,
//to nrdi pole vse cobra uporablja pa uni Use da ves ku se kljice....
func main() {
	cmd.Execute()
}
