package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/badoux/checkmail"
	"github.com/gorilla/mux"
	gocid "github.com/ipfs/go-cid"
	"github.com/tidwall/gjson"
)

func queryCIDs(w http.ResponseWriter, r *http.Request) {
	owner := mux.Vars(r)["owner"]
	cid := strings.TrimSpace(r.URL.Query().Get("cid"))
	if strings.HasPrefix(cid, "/ipfs/") {
		cid = cid[6:]
	}

	var err error

	match := ""
	args := []interface{}{cid}
	var entries []HistoryEntry

	if owner != "" {
		// just for one owner
		match += `AND head.owner = $2 `
		args = append(args, owner)
	}

	err = pg.Select(&entries, `
        SELECT owner, name, set_at, history.cid, (
          SELECT count(*) FROM history AS hc
          WHERE hc.record_id = history.record_id
            AND hc.set_at > history.set_at
        ) AS nseq
        FROM history 
        INNER JOIN head ON history.record_id = head.id
        WHERE history.cid = $1 `+match+`
        ORDER BY updated_at DESC
    `, args...)

	if err != nil && err != sql.ErrNoRows {
		log.Warn().Err(err).Str("owner", owner).Str("cid", cid).
			Msg("error fetching stuff from database")
		http.Error(w, "Error fetching data.", 500)
		return
	}

	if entries == nil {
		entries = make([]HistoryEntry, 0)
	}

	json.NewEncoder(w).Encode(entries)
}

func getUser(w http.ResponseWriter, r *http.Request) {
	owner := mux.Vars(r)["owner"]

	userInfo := UserInfo{Stars: []string{}}
	err := pg.Get(&userInfo, `
        SELECT name, string_agg(target_owner || '/' || target_name, ',') AS raw_stars
        FROM users
        LEFT OUTER JOIN stars ON stars.source = users.name
        WHERE name = $1
        GROUP BY name
    `, owner)

	if userInfo.RawStars.Valid {
		userInfo.Stars = strings.Split(userInfo.RawStars.String, ",")
	}

	if err != nil && err != sql.ErrNoRows {
		log.Warn().Err(err).Str("owner", owner).Msg("error fetching stuff from database")
		http.Error(w, "Error fetching data.", 500)
		return
	}

	json.NewEncoder(w).Encode(userInfo)
}

func listNames(w http.ResponseWriter, r *http.Request) {
	owner := mux.Vars(r)["owner"]

	var err error

	var entries []Entry
	if owner == "" {
		// all records globally
		err = pg.Select(&entries, `
            SELECT
              owner, name, cid, note,
              count(stars) AS nstars
            FROM head 
            LEFT OUTER JOIN stars
              ON target_owner = head.owner AND target_name = head.name
            GROUP BY owner, name, cid, note, updated_at
            ORDER BY updated_at DESC
        `)
	} else {
		// all records for just one user
		err = pg.Select(&entries, `
            SELECT
              owner, name, cid, note,
              count(stars) AS nstars
            FROM head
            LEFT OUTER JOIN stars
              ON target_owner = head.owner AND target_name = head.name
            WHERE owner = $1
            GROUP BY owner, name, cid, note, updated_at
            ORDER BY updated_at DESC
        `, owner)
	}

	if err != nil && err != sql.ErrNoRows {
		log.Warn().Err(err).Str("owner", owner).Msg("error fetching stuff from database")
		http.Error(w, "Error fetching data.", 500)
		return
	}

	if entries == nil {
		entries = make([]Entry, 0)
	}

	json.NewEncoder(w).Encode(entries)
}

func getName(w http.ResponseWriter, r *http.Request) {
	owner := mux.Vars(r)["owner"]
	name := mux.Vars(r)["name"]

	// show specific key
	query := `
        WITH st AS (
          SELECT count(*) AS nstars FROM stars
          WHERE target_owner = $1 AND target_name = $2
        )
        SELECT owner, name, cid, note, nstars
        FROM head, st
        WHERE owner = $1 AND name = $2
    `
	if r.URL.Query().Get("full") == "1" {
		query = `
            WITH df AS (
              SELECT id AS rid, owner, name, cid, note, body
              FROM head
              WHERE owner = $1 AND name = $2
            ), ph AS (
              SELECT array_agg(cid || '|' || set_at ORDER BY id DESC) AS r
              FROM history
              WHERE record_id = (SELECT rid FROM df)
            ), st AS (
              SELECT count(*) AS nstars FROM stars
              WHERE target_owner = $1 AND target_name = $2
            )
            SELECT
              owner, name, cid, note, body,
              array_to_string(r, '~') AS raw_history,
              nstars
            FROM df, ph, st;
        `
	}

	var entry Entry
	err = pg.Get(&entry, query, owner, name)
	res := &entry
	if err == sql.ErrNoRows {
		res = nil
		goto end
	} else if err != nil && err != sql.ErrNoRows {
		log.Warn().Err(err).Str("owner", owner).Str("name", name).
			Msg("error fetching stuff from database")
		http.Error(w, "Error fetching data.", 500)
		return
	}

	if res.RawHistory.Valid {
		hentries := strings.Split(res.RawHistory.String, "~")
		res.History = make([]HistoryEntry, len(hentries))
		for i, hentry := range hentries {
			parts := strings.Split(hentry, "|")
			res.History[i] = HistoryEntry{
				CID:  parts[0],
				Date: parts[1],
			}
		}
	}

end:
	json.NewEncoder(w).Encode(res)
}

func redirectName(w http.ResponseWriter, r *http.Request) {
	owner := mux.Vars(r)["owner"]
	name := mux.Vars(r)["name"]

	var cid string
	err = pg.Get(&cid, `
        SELECT cid FROM head
        WHERE owner = $1 AND name = $2
    `, owner, name)
	if err == sql.ErrNoRows {
		http.Error(w, "Couldn't find object.", 404)
		return
	}

	http.Redirect(w, r, "https://cloudflare-ipfs.com/ipfs/"+cid, 302)
}

func registerUser(w http.ResponseWriter, r *http.Request) {
	owner := mux.Vars(r)["owner"]
	email := r.Header.Get("Email")
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Missing public key.", 400)
		return
	}
	pk := string(data)

	// register a new user at /owner
	if err := checkmail.ValidateFormat(email); err != nil {
		log.Warn().Err(err).Str("email", email).
			Msg("invalid email address")
		http.Error(w, "Invalid email address: "+err.Error(), 400)
		return
	}

	_, err = pg.Exec(`
        INSERT INTO users (name, email, pk)
        VALUES ($1, $2, $3)
    `, owner, email, pk)

	if err != nil {
		log.Warn().Err(err).Str("owner", owner).Str("email", email).
			Msg("error creating user")
		http.Error(w, "Error creating user: "+err.Error(), 500)
		return
	}

	w.WriteHeader(200)
}

func updateUser(w http.ResponseWriter, r *http.Request) {
	owner := mux.Vars(r)["owner"]

	var data map[string]interface{}
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		http.Error(w, "Invalid JSON body.", 400)
		return
	}

	token := r.Header.Get("Token")
	err = validateJWT(token, owner, map[string]interface{}{
		"owner": owner,
	})
	if err != nil {
		log.Warn().Err(err).Str("token", token).Msg("token data is invalid")
		http.Error(w, "Token data is invalid: "+err.Error(), 401)
		return
	}

	if target, ok := data["star"]; ok {
		// special case: star
		delete(data, "star")
		parts := strings.Split(target.(string), "/")
		target_owner := parts[0]
		target_name := parts[1]
		_, err = pg.Exec(`
            INSERT INTO stars (source, target_owner, target_name)
            VALUES ($1, $2, $3)
            ON CONFLICT (source, target_owner, target_name) DO NOTHING
        `, owner, target_owner, target_name)
	} else if target, ok := data["unstar"]; ok {
		// special case: unstar
		delete(data, "unstar")
		parts := strings.Split(target.(string), "/")
		target_owner := parts[0]
		target_name := parts[1]
		_, err = pg.Exec(`
            DELETE FROM stars
            WHERE source = $1
              AND target_owner = $2 AND target_name = $3
        `, owner, target_owner, target_name)
	} else {
		setKeys := make([]string, len(data))
		setValues := make([]interface{}, len(data)+1)
		setValues[0] = owner
		i := 0
		for k, v := range data {
			setKeys[i] = fmt.Sprintf("%s = $%v", k, i+2)
			setValues[i+1] = v
			i++
		}
		_, err = pg.Exec(`
        UPDATE users SET
        `+strings.Join(setKeys, ", ")+`
        WHERE owner = $1
    `, setValues...)
	}

	if err != nil {
		log.Warn().Err(err).Str("owner", owner).Fields(data).
			Msg("error updating record")
		http.Error(w, "Error updating record: "+err.Error(), 500)
		return
	}

	w.WriteHeader(200)
}

func setName(w http.ResponseWriter, r *http.Request) {
	owner := mux.Vars(r)["owner"]
	name := mux.Vars(r)["name"]

	token := r.Header.Get("Token")
	err = validateJWT(token, owner, map[string]interface{}{
		"owner": owner,
		"name":  name,
	})
	if err != nil {
		log.Warn().Err(err).Str("owner", owner).Str("name", name).
			Str("token", token).
			Msg("token data is invalid")
		http.Error(w, "Token data is invalid: "+err.Error(), 401)
		return
	}

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Missing request body.", 400)
		return
	}

	values := gjson.GetManyBytes(data, "cid", "note")
	cid := values[0].String()
	note := values[1].String()

	// check cid validity
	if pcid, err := gocid.Parse(cid); err != nil {
		http.Error(w, "Invalid CID.", 400)
		return
	} else {
		cid = pcid.String()
	}

	var id string
	err = pg.Get(&id, `
        INSERT INTO head (owner, name, cid, note)
        VALUES ($1, $2, $3, $4)
        ON CONFLICT (owner, name) DO
        UPDATE SET
          cid = $3,
          note = CASE WHEN character_length($4) > 0 THEN $4 ELSE head.note END,
          updated_at = now()
        RETURNING id::text
    `, owner, name, cid, note)
	if err != nil {
		log.Warn().Err(err).Str("owner", owner).Str("name", name).
			Msg("error upserting record")
		http.Error(w, "Error upserting record: "+err.Error(), 500)
		return
	}

	// dispatch to activitypub
	log.Print(id, " ", owner, " ", name, " ", cid)
	go pubDispatchNote(id, owner, name, cid)

	w.WriteHeader(200)
}

func updateName(w http.ResponseWriter, r *http.Request) {
	owner := mux.Vars(r)["owner"]
	name := mux.Vars(r)["name"]

	token := r.Header.Get("Token")
	err = validateJWT(token, owner, map[string]interface{}{
		"owner": owner,
		"name":  name,
	})
	if err != nil {
		log.Warn().Err(err).Str("owner", owner).Str("name", name).
			Str("token", token).
			Msg("token data is invalid")
		http.Error(w, "Token data is invalid: "+err.Error(), 401)
		return
	}

	var data map[string]interface{}
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		http.Error(w, "Invalid JSON body.", 400)
		return
	}

	setKeys := make([]string, len(data))
	setValues := make([]interface{}, len(data)+2)
	setValues[0] = owner
	setValues[1] = name
	i := 0
	for k, v := range data {
		setKeys[i] = fmt.Sprintf("%s = $%v", k, i+3)
		setValues[i+2] = v
		i++
	}

	_, err = pg.Exec(`
        UPDATE head SET
        `+strings.Join(setKeys, ", ")+`
        WHERE owner = $1 AND name = $2
    `, setValues...)
	if err != nil {
		log.Warn().Err(err).Str("owner", owner).Str("name", name).
			Msg("error updating record")
		http.Error(w, "Error updating record: "+err.Error(), 500)
		return
	}

	w.WriteHeader(200)
}

func delName(w http.ResponseWriter, r *http.Request) {
	owner := mux.Vars(r)["owner"]
	name := mux.Vars(r)["name"]

	token := r.Header.Get("Token")
	err := validateJWT(token, owner, map[string]interface{}{
		"owner": owner,
		"name":  name,
	})
	if err != nil {
		log.Warn().Err(err).Str("owner", owner).Str("name", name).
			Str("token", token).
			Msg("token data is invalid")
		http.Error(w, "Token data is invalid: "+err.Error(), 401)
		return
	}

	_, err = pg.Exec(`
        DELETE FROM head
        WHERE owner = $1 AND name = $2
    `, owner, name)

	if err != nil {
		log.Warn().Err(err).Str("owner", owner).Str("name", name).
			Msg("error updating record")
		http.Error(w, "Error updating record: "+err.Error(), 500)
		return
	}

	w.WriteHeader(200)
}
