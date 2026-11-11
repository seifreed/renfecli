package client

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestJSObjectToJSON(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"bare keys", `{a:1,b:"x"}`, `{"a":1,"b":"x"}`},
		{"nested", `{a:{b:[1,2]},c:null}`, `{"a":{"b":[1,2]},"c":null}`},
		// A comma inside an array must not put the scanner in key position, or
		// the next bare word would be quoted as if it were a field name.
		{"array of words", `{a:[true,false,null]}`, `{"a":[true,false,null]}`},
		{"array of objects", `{a:[{b:1},{b:2}]}`, `{"a":[{"b":1},{"b":2}]}`},
		// The values Renfe actually sends contain colons, braces and commas.
		{"punctuation in string", `{h:"09:50",n:"AVE, {directo}"}`, `{"h":"09:50","n":"AVE, {directo}"}`},
		{"escaped quote", `{n:"a\"b:c"}`, `{"n":"a\"b:c"}`},
		{"already quoted key", `{"a":1}`, `{"a":1}`},
		{"underscore and digits", `{a_1:1,$b:2}`, `{"a_1":1,"$b":2}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := jsObjectToJSON(tc.in)
			if err != nil {
				t.Fatalf("jsObjectToJSON(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("jsObjectToJSON(%q)\n got %s\nwant %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestJSObjectToJSONRejectsMalformed(t *testing.T) {
	for _, in := range []string{`{a:"unterminated`, `{a:1`, `{a:1}}`} {
		if _, err := jsObjectToJSON(in); err == nil {
			t.Errorf("jsObjectToJSON(%q) = nil error, want failure", in)
		}
	}
}

func TestDecodeDWRExceptionIsAnError(t *testing.T) {
	script := `throw 'x';
(function(){
var r=window.dwr._[0];
r.handleException("0","0",{javaClassName:"java.lang.RuntimeException",message:"sesion caducada"});
})();`
	var out map[string]any
	err := decodeDWR(script, &out)
	if err == nil {
		t.Fatal("decodeDWR on an exception reply = nil error, want failure")
	}
	if strings.Contains(err.Error(), "sesion caducada") || err.Error() != "Renfe returned an application error" {
		t.Errorf("error should hide the remote response body, got %v", err)
	}
}

func TestDecodeDWRUnexpectedResponseDoesNotExposeBody(t *testing.T) {
	const secret = "private-response-content"
	var out map[string]any
	err := decodeDWR("<html>"+secret+"</html>", &out)
	if err == nil || errors.Is(err, ErrQueued) || strings.Contains(err.Error(), secret) {
		t.Fatalf("unexpected response error leaked body or lost type: %v", err)
	}
}

// Not every bean answers with an object: the signed-in user's name comes back
// as a bare string literal, and a decoder that only bracket-matches would call
// that a truncated payload.
func TestDecodeDWRBareStringPayload(t *testing.T) {
	script := `r.handleCallback("0","0","MARC RIVERO");`
	var name string
	if err := decodeDWR(script, &name); err != nil {
		t.Fatalf("decodeDWR: %v", err)
	}
	if name != "MARC RIVERO" {
		t.Errorf("name = %q", name)
	}
}

func TestDecodeDWREmptyArrayPayload(t *testing.T) {
	var trips []Trip
	if err := decodeDWR(`r.handleCallback("0","0",[]);`, &trips); err != nil {
		t.Fatalf("decodeDWR: %v", err)
	}
	if len(trips) != 0 {
		t.Errorf("an account with nothing booked should decode to an empty slice, got %v", trips)
	}
}

func TestDecodeDWRRejectsNonDWRBody(t *testing.T) {
	var out map[string]any
	if err := decodeDWR("<html>queue-it</html>", &out); err == nil {
		t.Fatal("decodeDWR on an HTML body = nil error, want failure")
	}
}

// TestDecodeDWRFixture parses a real getTrainsList reply captured from
// venta.renfe.com (Madrid → Barcelona) and checks the fields the CLI prints.
func TestDecodeDWRFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/trainslist.dwr")
	if err != nil {
		t.Fatal(err)
	}
	var reply trainsListReply
	if err := decodeDWR(string(raw), &reply); err != nil {
		t.Fatalf("decodeDWR: %v", err)
	}
	if len(reply.ListadoTrenes) == 0 {
		t.Fatal("no legs decoded")
	}
	res, err := reply.results(Query{Origin: "60000", Destination: "71801"})
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if res.From != "MADRID-PUERTA DE ATOCHA-ALMUDENA GRANDES" || res.To != "BARCELONA-SANTS" {
		t.Errorf("route = %q → %q", res.From, res.To)
	}
	if len(res.Journeys) < 20 {
		t.Fatalf("got %d journeys, want the full day's service", len(res.Journeys))
	}
	// Journeys must come back in departure order regardless of the API's order.
	for i := 1; i < len(res.Journeys); i++ {
		if res.Journeys[i-1].Departure > res.Journeys[i].Departure {
			t.Fatalf("journeys out of order at %d: %s then %s",
				i, res.Journeys[i-1].Departure, res.Journeys[i].Departure)
		}
	}
	var found bool
	for _, j := range res.Journeys {
		if j.Departure != "06:27" {
			continue
		}
		found = true
		if j.Arrival != "09:50" || j.Minutes != 203 || j.TrainType != "AVE" {
			t.Errorf("06:27 journey = %+v", j)
		}
		if len(j.Trains) != 1 || j.Trains[0] != "3063" {
			t.Errorf("train numbers = %v, want [3063] (leading zeros trimmed)", j.Trains)
		}
		if j.Price != 49.8 {
			t.Errorf("price = %v, want 49.8", j.Price)
		}
		if !j.Available {
			t.Error("a journey quoting fares should be available")
		}
		if len(j.Fares) != 4 || j.Fares[0].Name != "Básico" || j.Fares[3].Price != 91.85 {
			t.Errorf("fares = %+v", j.Fares)
		}
	}
	if !found {
		t.Error("the 06:27 AVE is missing from the results")
	}
	if len(res.Calendar) == 0 {
		t.Error("price calendar was dropped")
	}
}

// A round trip needs no second request: the one reply carries both directions,
// keyed by viajeIda.
func TestDecodeDWRRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("testdata/roundtrip.dwr")
	if err != nil {
		t.Fatal(err)
	}
	var reply trainsListReply
	if err := decodeDWR(string(raw), &reply); err != nil {
		t.Fatalf("decodeDWR: %v", err)
	}
	res, err := reply.results(Query{Origin: "MADRI", Destination: "BARCE"})
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if res.From != "MADRID-PUERTA DE ATOCHA-ALMUDENA GRANDES" || res.Date != "22/09/2026" {
		t.Errorf("outbound = %s on %s", res.From, res.Date)
	}
	if res.Return == nil {
		t.Fatal("the return leg was dropped")
	}
	// The return leg must be the reversed route on the later date, not a copy.
	if res.Return.From != res.To || res.Return.To != res.From {
		t.Errorf("return leg = %s → %s, want the reverse of %s → %s",
			res.Return.From, res.Return.To, res.From, res.To)
	}
	if res.Return.Date != "25/09/2026" {
		t.Errorf("return date = %s, want 25/09/2026", res.Return.Date)
	}
	if len(res.Journeys) == 0 || len(res.Return.Journeys) == 0 {
		t.Errorf("journeys: %d out, %d back", len(res.Journeys), len(res.Return.Journeys))
	}
}

// A one-way search still gets an empty second leg from Renfe; it must not show
// up as a return the user never asked for.
func TestDecodeDWROneWayHasNoReturn(t *testing.T) {
	raw, err := os.ReadFile("testdata/trainslist.dwr")
	if err != nil {
		t.Fatal(err)
	}
	var reply trainsListReply
	if err := decodeDWR(string(raw), &reply); err != nil {
		t.Fatal(err)
	}
	res, err := reply.results(Query{Origin: "60000", Destination: "71801"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Return != nil {
		t.Errorf("one-way search produced a return leg with %d journeys", len(res.Return.Journeys))
	}
}

// Renfe says nothing but an empty list when a date is outside its sale window,
// so the error has to supply the explanation — and name the stations the user
// typed, not the internal codes.
func TestResultsEmptyListIsAnExplainedError(t *testing.T) {
	var reply trainsListReply
	if err := decodeDWR(`r.handleCallback("0","0",{listadoTrenes:[]});`, &reply); err != nil {
		t.Fatal(err)
	}
	_, err := reply.results(Query{
		Origin: "MADRI", OriginName: "MADRID (TODAS)",
		Destination: "VIGO-", DestName: "VIGO (TODAS)",
		Date: time.Date(2027, 4, 8, 0, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatal("an empty list should be an error")
	}
	for _, want := range []string{"MADRID (TODAS)", "VIGO (TODAS)", "2027-04-08", "sale window"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// Renfe returns the same empty list when a filter excluded everything as when
// the date is unsellable. Blaming the sale window for a --bike search sends the
// user to look at the wrong thing, so active filters are named first.
func TestResultsEmptyBlamesTheActiveFilter(t *testing.T) {
	var reply trainsListReply
	if err := decodeDWR(`r.handleCallback("0","0",{listadoTrenes:[]});`, &reply); err != nil {
		t.Fatal(err)
	}
	q := Query{
		Origin: "MADRI", OriginName: "MADRID (TODAS)",
		Destination: "BARCE", DestName: "BARCELONA (TODAS)",
		Date: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC),
		Bike: true, Pet: true,
	}
	_, err := reply.results(q)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"--bike", "--pet", "those filters"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}

	q.Bike, q.Pet = false, false
	if _, err = reply.results(q); err == nil || strings.Contains(err.Error(), "--") {
		t.Errorf("with no filters set the error should not name one: %v", err)
	}
}

func TestDWRCallBody(t *testing.T) {
	body := dwrCall("trainEnlacesManager", "getTrainsList", "/vol/buscarTrenEnlaces.do", "abc/renfecli", []DWRParam{
		dwrStr("fechaSalida", "22/09/2026"),
		dwrStr("adultos", "1"),
	})
	for _, want := range []string{
		"c0-scriptName=trainEnlacesManager\n",
		"c0-methodName=getTrainsList\n",
		// Slashes in a date must be percent-encoded, as DWR's own client does.
		"c0-e1=string:22%2F09%2F2026\n",
		"c0-e2=string:1\n",
		"c0-param0=Object_Object:{fechaSalida:reference:c0-e1, adultos:reference:c0-e2}\n",
		"page=%2Fvol%2FbuscarTrenEnlaces.do\n",
		"scriptSessionId=abc/renfecli\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// A zero-argument bean (the account lookups) must not send an empty object
// argument — DWR answers a method-signature mismatch with an exception.
func TestDWRCallWithoutParams(t *testing.T) {
	body := dwrCall("sesionManager", "getIdentificacionUsuario", "/vol/homeCustomers.do", "abc/renfecli", nil)
	if strings.Contains(body, "c0-param0") {
		t.Errorf("zero-arg call sent a param:\n%s", body)
	}
	if !strings.Contains(body, "c0-methodName=getIdentificacionUsuario\n") {
		t.Errorf("body missing the method name:\n%s", body)
	}
}

// DWR treats the scriptSessionId as a CSRF token: it must carry the
// DWRSESSIONID cookie as its prefix, or the server refuses the call.
func TestScriptSessionIDBindsToCookie(t *testing.T) {
	if got := scriptSessionID("ABC123"); got != "ABC123/"+dwrPageToken {
		t.Errorf("scriptSessionID = %q, want the cookie as prefix", got)
	}
	if got := scriptSessionID(""); got != dwrAnonScriptSessionID {
		t.Errorf("without a cookie, scriptSessionID = %q", got)
	}
}

func TestDWREscapeKeepsSpacesPercentEncoded(t *testing.T) {
	// '+' would reach the Java decoder as a literal plus, not a space.
	if got := dwrEscape("BARCELONA SANTS"); got != "BARCELONA%20SANTS" {
		t.Errorf("dwrEscape = %q", got)
	}
}

func TestDWRPrice(t *testing.T) {
	for in, want := range map[string]float64{
		"49,8": 49.8, "1.234,50": 1234.5, "91,85": 91.85, "": 0, "n/d": 0, "17": 17,
	} {
		if got := dwrPrice(in); got != want {
			t.Errorf("dwrPrice(%q) = %v, want %v", in, got, want)
		}
	}
}

// Renfe fronts the site with Queue-it when demand spikes. The body that comes
// back is HTML, so without this the caller sees "not a DWR reply: <!DOCTYPE…"
// and has no idea it is simply queued.
func TestDecodeDWRDetectsTheWaitingRoom(t *testing.T) {
	page := `<!DOCTYPE html><html><head>` +
		`<script src="//static.queue-it.net/script/queueclient.min.js"></script>` +
		`</head><body>You are now in line…</body></html>`
	var out map[string]any
	err := decodeDWR(page, &out)
	if !errors.Is(err, ErrQueued) {
		t.Fatalf("err = %v, want ErrQueued so callers can tell a queue from a parse failure", err)
	}
	if !strings.Contains(err.Error(), "waiting room") {
		t.Errorf("the message should say what happened, got %q", err)
	}
}

// A real DWR reply must never be mistaken for a queue page, even though the
// booking pages themselves load the Queue-it client script.
func TestDecodeDWRQueueCheckDoesNotFireOnRealReplies(t *testing.T) {
	var name string
	if err := decodeDWR(`r.handleCallback("0","0","queue-it.net");`, &name); err != nil {
		t.Fatalf("decodeDWR: %v", err)
	}
	if name != "queue-it.net" {
		t.Errorf("name = %q", name)
	}
}

// A connection that changes station — crossing Madrid from Atocha to Chamartín
// — makes an hour of "transfer time" mean something entirely different. Renfe
// warns about it in its own field, which the CLI used to drop.
func TestStationChangeWarningIsSurfaced(t *testing.T) {
	reply := `r.handleCallback("0","0",{listadoTrenes:[{viajeIda:true,hayError:false,` +
		`descripcionEstacionOrigen:"BARCELONA-SANTS",descripcionEstacionDestino:"SANTANDER",` +
		`fechaOrigen:"23/09/2026",listviajeViewEnlaceBean:[{horaSalida:"08:00",horaLlegada:"17:47",` +
		`directo:false,duracionTrasbordo:"1 horas 56 minutos",tarifaMinima:"133,90",` +
		`msgCambioEstacionTransbordo:"ATENCIÓN: Necesario cambio de estación.<br><br>Entre A y B.",` +
		`trayectos:[{cdgoTren:"03082"},{cdgoTren:"04143"}],tarifasDisponibles:[]}]}]});`
	var rep trainsListReply
	if err := decodeDWR(reply, &rep); err != nil {
		t.Fatal(err)
	}
	res, err := rep.results(Query{Origin: "71801", Destination: "15410"})
	if err != nil {
		t.Fatal(err)
	}
	j := res.Journeys[0]
	if j.StationChange == "" {
		t.Fatal("the station-change warning was dropped")
	}
	// The HTML Renfe embeds must be flattened, not printed raw.
	if strings.Contains(j.StationChange, "<br>") {
		t.Errorf("markup left in the message: %q", j.StationChange)
	}
	if !strings.Contains(j.StationChange, "Necesario cambio de estación") {
		t.Errorf("message = %q", j.StationChange)
	}
}

// A direct train has no connection, so it must never carry the warning.
func TestDirectJourneyHasNoStationChange(t *testing.T) {
	raw, err := os.ReadFile("testdata/trainslist.dwr")
	if err != nil {
		t.Fatal(err)
	}
	var rep trainsListReply
	if err := decodeDWR(string(raw), &rep); err != nil {
		t.Fatal(err)
	}
	res, err := rep.results(Query{Origin: "60000", Destination: "71801"})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range res.Journeys {
		if j.Direct && (j.StationChange != "" || j.TransferTime != "") {
			t.Errorf("direct journey %s carries connection data: %+v", j.Departure, j)
		}
	}
}

func TestPlainText(t *testing.T) {
	for in, want := range map[string]string{
		"a<br><br>b":  "a b",
		"a<br/>b":     "a b",
		"  a   b  ":   "a b",
		"":            "",
		"sin marcado": "sin marcado",
	} {
		if got := plainText(in); got != want {
			t.Errorf("plainText(%q) = %q, want %q", in, got, want)
		}
	}
}

// A train down to its last wheelchair spaces still quotes a fare, so it used to
// read as ordinary availability to someone who cannot buy it.
func TestWheelchairOnlyIsNotAvailableToEveryone(t *testing.T) {
	reply := `r.handleCallback("0","0",{listadoTrenes:[{viajeIda:true,hayError:false,` +
		`descripcionEstacionOrigen:"A",descripcionEstacionDestino:"B",fechaOrigen:"22/09/2026",` +
		`listviajeViewEnlaceBean:[{horaSalida:"07:27",horaLlegada:"11:11",directo:true,` +
		`soloPlazaH:true,completo:false,tarifaMinima:"49,8",trayectos:[{cdgoTren:"03073"}],` +
		`tarifasDisponibles:[]}]}]});`
	var rep trainsListReply
	if err := decodeDWR(reply, &rep); err != nil {
		t.Fatal(err)
	}

	res, err := rep.results(Query{Origin: "A", Destination: "B"})
	if err != nil {
		t.Fatal(err)
	}
	j := res.Journeys[0]
	if !j.WheelchairOnly {
		t.Error("soloPlazaH was dropped")
	}
	if j.Available {
		t.Error("a wheelchair-only train is not available to a traveller who did not ask for an H space")
	}
	if j.Price != 49.8 {
		t.Errorf("the fare is still real: %v", j.Price)
	}

	// Someone who asked for an H space CAN buy it.
	res, err = rep.results(Query{Origin: "A", Destination: "B", WheelchairH: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Journeys[0].Available {
		t.Error("with --wheelchair the train is available")
	}
}

// Price is documented as the cheapest fare on the journey. When Renfe sends no
// tarifaMinima the code falls back to the fare list, and taking the first entry
// makes that a promise it does not keep: the list is Renfe's, in Renfe's order.
func TestPriceFallsBackToTheCheapestFareNotTheFirst(t *testing.T) {
	script := jsReply(`{listadoTrenes:[{viajeIda:true,descripcionEstacionOrigen:"MADRID",
		descripcionEstacionDestino:"BARCELONA",fechaOrigen:"04/10/2026",
		listviajeViewEnlaceBean:[{horaSalida:"06:00",horaLlegada:"09:00",tarifaMinima:"",
			trayectos:[{cdgoTren:"03311"}],
			tarifasDisponibles:[{codigoTarifa:"P",titulo:"Prémium",precioTarifa:"120,00",cdgoClase:"P"},
				{codigoTarifa:"B",titulo:"Básico",precioTarifa:"45,00",cdgoClase:"T"}]}]}]}`)
	var reply trainsListReply
	if err := decodeDWR(script, &reply); err != nil {
		t.Fatal(err)
	}
	res, err := reply.results(Query{Origin: "60000", Destination: "71801"})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Journeys[0].Price; got != 45 {
		t.Errorf("Price = %.2f, want the cheapest fare 45.00 (Renfe listed Prémium first)", got)
	}
}

// A round trip whose return day has nothing on it dropped the return leg
// entirely, so the caller saw a one-way result and no word about the half they
// asked for.
func TestAskedForAReturnGetsOneEvenWhenItIsEmpty(t *testing.T) {
	script := jsReply(`{listadoTrenes:[
		{viajeIda:true,descripcionEstacionOrigen:"MADRID",descripcionEstacionDestino:"BARCELONA",
		 fechaOrigen:"04/10/2026",listviajeViewEnlaceBean:[{horaSalida:"06:00",horaLlegada:"09:00",
		 tarifaMinima:"50,00",trayectos:[{cdgoTren:"03311"}]}]},
		{viajeIda:false,descripcionEstacionOrigen:"BARCELONA",descripcionEstacionDestino:"MADRID",
		 fechaOrigen:"06/10/2026",listviajeViewEnlaceBean:[]}]}`)
	var reply trainsListReply
	if err := decodeDWR(script, &reply); err != nil {
		t.Fatal(err)
	}
	asked := Query{Origin: "60000", Destination: "71801", Return: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}
	res, err := reply.results(asked)
	if err != nil {
		t.Fatal(err)
	}
	if res.Return == nil {
		t.Fatal("a round trip must report its return leg even when the day is empty")
	}
	if len(res.Return.Journeys) != 0 {
		t.Errorf("return journeys = %d, want none", len(res.Return.Journeys))
	}

	// A one-way must still not grow a return: Renfe sends the empty leg anyway.
	oneWay, err := reply.results(Query{Origin: "60000", Destination: "71801"})
	if err != nil {
		t.Fatal(err)
	}
	if oneWay.Return != nil {
		t.Error("a one-way search picked up Renfe's empty return leg")
	}
}

// jsReply wraps a DWR object literal as the script the endpoint returns.
func jsReply(payload string) string {
	return "throw 'x';\n(function(){\nvar r=window.dwr._[0];\n" +
		`r.handleCallback("0","0",` + payload + ");\n})();"
}
