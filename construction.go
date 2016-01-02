package htmlhouse

import (
	"bytes"
	"database/sql"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/writeas/impart"
	"github.com/writeas/nerds/store"
	"github.com/writeas/web-core/bots"
)

func createHouse(app *app, w http.ResponseWriter, r *http.Request) error {
	html := r.FormValue("html")
	if strings.TrimSpace(html) == "" {
		return impart.HTTPError{http.StatusBadRequest, "Supply something to publish."}
	}
	public := r.FormValue("public") == "true"

	houseID := store.GenerateFriendlyRandomString(8)

	_, err := app.db.Exec("INSERT INTO houses (id, html) VALUES (?, ?)", houseID, html)
	if err != nil {
		return err
	}

	if err = app.session.writeToken(w, houseID); err != nil {
		return err
	}

	resUser := newSessionInfo(houseID)

	if public {
		go addPublicAccess(app, houseID, html)
	}

	return impart.WriteSuccess(w, resUser, http.StatusCreated)
}

func validTitle(title string) bool {
	return title != "" && strings.TrimSpace(title) != "HTMLhouse"
}

func removePublicAccess(app *app, houseID string) error {
	var approved sql.NullInt64
	err := app.db.QueryRow("SELECT approved FROM houses WHERE id = ?", houseID).Scan(&approved)
	switch {
	case err == sql.ErrNoRows:
		return nil
	case err != nil:
		fmt.Printf("Couldn't fetch for public removal: %v\n", err)
		return nil
	}

	if approved.Valid && approved.Int64 == 0 {
		// Page has been banned, so do nothing
	} else {
		_, err = app.db.Exec("DELETE FROM publichouses WHERE house_id = ?", houseID)
		if err != nil {
			return err
		}
	}

	return nil
}

func addPublicAccess(app *app, houseID, html string) error {
	// Parse title of page
	title := titleReg.FindStringSubmatch(html)[1]
	if !validTitle(title) {
		// <title/> was invalid, so look for an <h1/>
		header := headerReg.FindStringSubmatch(html)[1]
		if validTitle(header) {
			// <h1/> was valid, so use that instead of <title/>
			title = header
		}
	}
	title = strings.TrimSpace(title)

	// Get thumbnail
	data := url.Values{}
	data.Set("url", fmt.Sprintf("%s/%s.html", app.cfg.HostName, houseID))

	u, err := url.ParseRequestURI("http://peeper.html.house")
	u.Path = "/"
	urlStr := fmt.Sprintf("%v", u)

	client := &http.Client{}
	r, err := http.NewRequest("POST", urlStr, bytes.NewBufferString(data.Encode()))
	if err != nil {
		fmt.Printf("Error creating request: %v", err)
	}
	r.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Add("Content-Length", strconv.Itoa(len(data.Encode())))

	var thumbURL string
	resp, err := client.Do(r)
	if err != nil {
		fmt.Printf("Error requesting thumbnail: %v", err)
		return impart.HTTPError{http.StatusInternalServerError, "Couldn't generate thumbnail"}
	} else {
		defer resp.Body.Close()
		body, _ := ioutil.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusOK {
			thumbURL = string(body)
		}
	}

	// Add to public houses table
	approved := sql.NullInt64{Valid: false}
	if app.cfg.AutoApprove {
		approved.Int64 = 1
		approved.Valid = true
	}
	_, err = app.db.Exec("INSERT INTO publichouses (house_id, title, thumb_url, added, updated, approved) VALUES (?, ?, ?, NOW(), NOW(), ?) ON DUPLICATE KEY UPDATE title = ?, updated = NOW()", houseID, title, thumbURL, approved, title)
	if err != nil {
		return err
	}

	return nil
}

func renovateHouse(app *app, w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	houseID := vars["house"]
	html := r.FormValue("html")
	if strings.TrimSpace(html) == "" {
		return impart.HTTPError{http.StatusBadRequest, "Supply something to publish."}
	}
	public := r.FormValue("public") == "true"

	authHouseID, err := app.session.readToken(r)
	if err != nil {
		return err
	}

	if authHouseID != houseID {
		return impart.HTTPError{http.StatusUnauthorized, "Bad token for this ⌂ house ⌂."}
	}

	_, err = app.db.Exec("UPDATE houses SET html = ? WHERE id = ?", html, houseID)
	if err != nil {
		return err
	}

	if err = app.session.writeToken(w, houseID); err != nil {
		return err
	}

	resUser := newSessionInfo(houseID)

	if public {
		go addPublicAccess(app, houseID, html)
	} else {
		go removePublicAccess(app, houseID)
	}

	return impart.WriteSuccess(w, resUser, http.StatusOK)
}

func getHouseStats(app *app, houseID string) (*time.Time, int64, error) {
	var created time.Time
	var views int64
	err := app.db.QueryRow("SELECT created, view_count FROM houses WHERE id = ?", houseID).Scan(&created, &views)
	switch {
	case err == sql.ErrNoRows:
		return nil, 0, impart.HTTPError{http.StatusNotFound, "Return to sender. Address unknown."}
	case err != nil:
		fmt.Printf("Couldn't fetch: %v\n", err)
		return nil, 0, err
	}

	return &created, views, nil
}

func getHouseHTML(app *app, houseID string) (string, error) {
	var html string
	err := app.db.QueryRow("SELECT html FROM houses WHERE id = ?", houseID).Scan(&html)
	switch {
	case err == sql.ErrNoRows:
		return "", impart.HTTPError{http.StatusNotFound, "Return to sender. Address unknown."}
	case err != nil:
		fmt.Printf("Couldn't fetch: %v\n", err)
		return "", err
	}

	return html, nil
}

// regular expressions for extracting data
var (
	htmlReg   = regexp.MustCompile("<html( ?.*)>")
	titleReg  = regexp.MustCompile("<title>(.+)</title>")
	headerReg = regexp.MustCompile("<h1>(.+)</h1>")
)

func getHouse(app *app, w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	houseID := vars["house"]

	// Fetch HTML
	html, err := getHouseHTML(app, houseID)
	if err != nil {
		if err, ok := err.(impart.HTTPError); ok {
			if err.Status == http.StatusNotFound {
				page, err := ioutil.ReadFile(app.cfg.StaticDir + "/404.html")
				if err != nil {
					page = []byte("<!DOCTYPE html><html><body>HTMLlot.</body></html>")
				}
				fmt.Fprintf(w, "%s", page)
				return nil
			}
		}
		return err
	}

	// Add nofollow meta tag
	if strings.Index(html, "<head>") == -1 {
		html = htmlReg.ReplaceAllString(html, "<html$1><head></head>")
	}
	html = strings.Replace(html, "<head>", "<head><meta name=\"robots\" content=\"nofollow\" />", 1)

	// Add links back to HTMLhouse
	homeLink := "<a href='/'>&lt;&#8962;/&gt;</a>"
	watermark := fmt.Sprintf("<div style='position: absolute;top:16px;right:16px;'>%s &middot; <a href='/stats/%s.html'>stats</a> &middot; <a href='/edit/%s.html'>edit</a></div>", homeLink, houseID, houseID)
	if strings.Index(html, "</body>") == -1 {
		html = strings.Replace(html, "</html>", "</body></html>", 1)
	}
	html = strings.Replace(html, "</body>", fmt.Sprintf("%s</body>", watermark), 1)

	// Print HTML, with sanity check in case someone did something crazy
	if strings.Index(html, homeLink) == -1 {
		fmt.Fprintf(w, "%s%s", html, watermark)
	} else {
		fmt.Fprintf(w, "%s", html)
	}

	if r.Method != "HEAD" && !bots.IsBot(r.UserAgent()) {
		app.db.Exec("UPDATE houses SET view_count = view_count + 1 WHERE id = ?", houseID)
	}
	return nil
}

func viewHouseStats(app *app, w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	houseID := vars["house"]

	created, views, err := getHouseStats(app, houseID)
	if err != nil {
		if err, ok := err.(impart.HTTPError); ok {
			if err.Status == http.StatusNotFound {
				// TODO: put this logic in one place (shared with getHouse func)
				page, err := ioutil.ReadFile(app.cfg.StaticDir + "/404.html")
				if err != nil {
					page = []byte("<!DOCTYPE html><html><body>HTMLlot.</body></html>")
				}
				fmt.Fprintf(w, "%s", page)
				return nil
			}
		}
		return err
	}

	viewsLbl := "view"
	if views != 1 {
		viewsLbl = "views"
	}
	app.templates["stats"].ExecuteTemplate(w, "stats", &HouseStats{
		ID: houseID,
		Stats: []Stat{
			Stat{
				Data:  fmt.Sprintf("%d", views),
				Label: viewsLbl,
			},
			Stat{
				Data:  created.Format(time.RFC1123),
				Label: "created",
			},
		},
	})

	return nil
}

func viewHouses(app *app, w http.ResponseWriter, r *http.Request) error {
	houses, err := getPublicHouses(app)
	if err != nil {
		fmt.Fprintf(w, ":(")
		return err
	}

	app.templates["browse"].ExecuteTemplate(w, "browse", struct{ Houses *[]PublicHouse }{houses})

	return nil
}

func getPublicHouses(app *app) (*[]PublicHouse, error) {
	houses := []PublicHouse{}
	rows, err := app.db.Query("SELECT house_id, title, thumb_url FROM publichouses WHERE approved = 1 ORDER BY updated DESC LIMIT 10")
	switch {
	case err == sql.ErrNoRows:
		return nil, impart.HTTPError{http.StatusNotFound, "Return to sender. Address unknown."}
	case err != nil:
		fmt.Printf("Couldn't fetch: %v\n", err)
		return nil, err
	}
	defer rows.Close()

	house := &PublicHouse{}
	for rows.Next() {
		err = rows.Scan(&house.ID, &house.Title, &house.ThumbURL)
		houses = append(houses, *house)
	}

	return &houses, nil
}

func isHousePublic(app *app, houseID string) bool {
	var dummy int64
	err := app.db.QueryRow("SELECT 1 FROM publichouses WHERE house_id = ?", houseID).Scan(&dummy)
	switch {
	case err == sql.ErrNoRows:
		return false
	case err != nil:
		fmt.Printf("Couldn't fetch: %v\n", err)
		return false
	}

	return true
}

func banHouse(app *app, w http.ResponseWriter, r *http.Request) error {
	houseID := r.FormValue("house")
	pass := r.FormValue("pass")
	if app.cfg.AdminPass != pass {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}

	_, err := app.db.Exec("UPDATE publichouses SET approved = 0 WHERE house_id = ?", houseID)
	if err != nil {
		fmt.Fprintf(w, "Couldn't ban house: %v", err)
		return err
	}

	fmt.Fprintf(w, "BOOM! %s banned.", houseID)
	return nil
}
