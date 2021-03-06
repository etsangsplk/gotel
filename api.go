package gotel

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"time"
)

// Response will hold a response sent back to the caller
type Response map[string]interface{}

type badGuest struct {
	App       string
	Component string
	NumFails  int64
}

type node struct {
	ID            int
	IPAddress     string
	NodeID        int
	IsCoordinator bool
}

var validTimeUnits = map[string]int{"seconds": 1, "minutes": 1, "hours": 1}

func writeError(w http.ResponseWriter, e interface{}) {
	w.WriteHeader(http.StatusBadRequest)
	w.Header().Set("Content-Type", "application/json")
	if bytes, err := json.Marshal(e); err != nil {
		_, err = w.Write([]byte("Could not encode error"))
		if err != nil {
			l.err("Could not encode error [%v]", err)
		}
	} else {
		_, err = w.Write(bytes)
		if err != nil {
			l.err("Could not write error [%v]", err)
		}
	}
}

func writeResponse(w http.ResponseWriter, e interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if bytes, err := json.Marshal(e); err != nil {
		l.err("Could not encode response [%v]", err)
		writeError(w, "Could not encode response")
	} else {
		_, err = w.Write(bytes)
		if err != nil {
			l.err("Could not write response [%v]", err)
		}
		return
	}
}

func (ge *Endpoint) makeReservation(w http.ResponseWriter, req *http.Request) {
	res := new(reservation)
	decoder := json.NewDecoder(req.Body)
	err := decoder.Decode(&res)
	if err != nil {
		l.err("Unable to accept reservation")
	}

	err = validateReservation(res)
	if err != nil {
		l.warn("Invalid reservations [%v]", res)
		writeError(w, fmt.Sprintf("Unable to store reservation, validation failure [%v]", err))
		return
	}

	l.info("%v", res)

	_, err = storeReservation(ge.Db, res)
	if err != nil {
		l.err("Unable to store reservation %v", res)
		writeError(w, "Unable to store reservation")
		return
	}
	writeResponse(w, "OK")
}

func (ge *Endpoint) getReservations() ([]reservation, error) {
	query := "SELECT id, app, component, owner, notify, alert_msg, frequency, time_units, last_checkin_timestamp, num_checkins FROM reservations ORDER BY last_checkin_timestamp DESC"
	rows, err := ge.Db.Query(query)
	if err != nil {
		return nil, err
	}
	reservations := []reservation{}
	defer rows.Close()
	for rows.Next() {
		var alertMessage sql.NullString
		res := reservation{}
		err = rows.Scan(&res.JobID, &res.App, &res.Component, &res.Owner, &res.Notify, &alertMessage, &res.Frequency,
			&res.TimeUnits, &res.LastCheckin, &res.NumCheckins)
		if err != nil {
			return nil, err
		}
		lastCheckin := time.Unix(res.LastCheckin, 0)
		res.TimeSinceLastCheckin = RelTime(lastCheckin, time.Now(), "ago", "")
		res.LastCheckinStr = lastCheckin.Format(time.RFC1123)
		if FailsSLA(res) {
			res.FailingSLA = true
		} else {
			res.FailingSLA = false
		}
		if (!alertMessage.Valid) || (alertMessage.String == "") {
			res.AlertMessage = alertMessage.String
		}
		reservations = append(reservations, res)
	}
	return reservations, nil
}

func (ge *Endpoint) getNodes() ([]node, error) {

	query := "SELECT id, ip_address, node_id FROM nodes ORDER BY id;"
	rows, err := ge.Db.Query(query)
	if err != nil {
		return nil, err
	}
	nodes := []node{}
	defer rows.Close()
	for rows.Next() {
		res := node{IsCoordinator: false}
		err = rows.Scan(&res.ID, &res.IPAddress, &res.NodeID)
		if err != nil {
			return nil, err
		}

		resp, err := http.Get(fmt.Sprintf("http://%s:8080/is-coordinator", res.IPAddress))
		if err != nil {
			l.warn("Unable to contact node [%s] assuming offline", res.IPAddress)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			l.warn("Didn't get a 200OK reply back from ip [%s]", res.IPAddress)
			continue
		}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			l.warn("Unable to read node response")
		}
		if string(body) == "true" {
			res.IsCoordinator = true
		}

		nodes = append(nodes, res)
	}
	return nodes, nil
}

func (ge *Endpoint) getBadGuests() ([]badGuest, error) {
	query := "SELECT app, component, count(*) AS cnt FROM alerts GROUP BY app, component ORDER by cnt DESC"
	rows, err := ge.Db.Query(query)
	if err != nil {
		return nil, err
	}
	guests := []badGuest{}
	defer rows.Close()
	for rows.Next() {
		res := badGuest{}
		err = rows.Scan(&res.App, &res.Component, &res.NumFails)
		if err != nil {
			return nil, err
		}
		guests = append(guests, res)
	}
	return guests, nil
}

func (ge *Endpoint) listReservations(w http.ResponseWriter, req *http.Request) {
	reservations, err := ge.getReservations()
	if err != nil {
		l.err("Unable to list reservations [%v]", err)
		r := Response{"success": false, "message": "Unable to list reservations"}
		writeResponse(w, r)
		return
	}
	result := Response{"success": true, "result": reservations}
	writeResponse(w, result)
	return
}

func (ge *Endpoint) doCheckin(w http.ResponseWriter, req *http.Request) {
	c := new(checkin)
	decoder := json.NewDecoder(req.Body)
	err := decoder.Decode(&c)
	if err != nil {
		l.err("Unable to accept checkin for %v", c)
		r := Response{"success": false, "message": "Unable to checkin: " + c.App}
		writeResponse(w, r)
		return
	}

	now := time.Now().UTC().Unix()

	_, err = storeCheckin(ge.Db, *c, now)
	if err != nil {
		l.err("Unable to save checkin for %v", c)
		r := Response{"success": false, "message": "Unable to save checkin: " + c.App}
		writeResponse(w, r)
		return
	}

	_, err = logHouseKeeping(ge.Db, *c, now)
	if err != nil {
		l.err("Unable to save checkin for %v", c)
		r := Response{"success": false, "message": "Unable to save checkin: " + c.App}
		writeResponse(w, r)
		return
	}
	l.info("app [%s] component [%s] checked in %v", c.App, c.Component, time.Now())
	r := Response{"success": true, "message": "Application checked in: " + c.App}
	writeResponse(w, r)
}

// used when you know your service will be offline for a bit and you want to pause alerts
func (ge *Endpoint) doSnooze(w http.ResponseWriter, req *http.Request) {
	p := new(snooze)
	decoder := json.NewDecoder(req.Body)
	err := decoder.Decode(&p)
	if err != nil {
		l.err("Unable to accept snooze for %v", p)
		r := Response{"success": false, "message": "Unable to snooze: " + p.App}
		writeResponse(w, r)
		return
	}

	err = validateSnooze(p)
	if err != nil {
		l.warn("Invalid reservations [%q]", p)
		writeError(w, fmt.Sprintf("Unable to store snooze, validation failure [%v]", err))
		return
	}

	_, err = storeSnooze(ge.Db, p)
	if err != nil {
		l.err("Unable to save snooze for %v", p)
		r := Response{"success": false, "message": "Unable to save snooze: " + p.App}
		writeResponse(w, r)
		return
	}

	r := Response{"success": true, "message": "Application alerting paused: " + p.App}
	writeResponse(w, r)

}

func validateSnooze(snooze *snooze) error {
	_, ok := validTimeUnits[snooze.TimeUnits]
	if !ok {
		return errors.New("Invalid time_units passed in")
	}
	if snooze.Duration == 0 {
		return errors.New("")
	}
	return nil
}

// used when you know your service will be offline for a bit and you want to pause alerts
func (ge *Endpoint) doCheckOut(w http.ResponseWriter, req *http.Request) {
	p := new(checkOut)
	decoder := json.NewDecoder(req.Body)
	err := decoder.Decode(&p)
	if err != nil {
		l.err("Unable to accept checkout for %v error [%s]", p, err)
		r := Response{"success": false, "message": "Unable to checkout: " + p.App}
		writeResponse(w, r)
		return
	}
	_, err = storeCheckOut(ge.Db, p)
	if err != nil {
		l.err("Unable to save checkout for %v", p)
		r := Response{"success": false, "message": "Unable to save checkout: " + p.App}
		writeResponse(w, r)
		return
	}
	r := Response{"success": true, "message": fmt.Sprintf("Application Removed [%s/%s] ", p.App, p.Component)}
	writeResponse(w, r)
}

func (ge *Endpoint) isCoordinator(w http.ResponseWriter, req *http.Request) {
	writeResponse(w, coordinator)
}

func validateReservation(res *reservation) error {
	timeUnits := map[string]int{"seconds": 1, "minutes": 1, "hours": 1}
	_, ok := timeUnits[res.TimeUnits]
	if !ok {
		return errors.New("Invalid time_units passed in")
	}
	return nil
}

// InitAPI initializes the webservice on the specific port
func (ge *Endpoint) InitAPI(port int, htmlPath string) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			r := Response{"success": true, "message": "A-OK!"}
			writeResponse(w, r)
		}
	})

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			reservations, err := ge.getReservations()

			if err != nil {
				l.err(err.Error())
				r := Response{"success": false, "message": "Unable to server views"}
				writeResponse(w, r)
			} else {
				t, err := template.ParseFiles(htmlPath + "/public/view.html")
				if err != nil {
					l.err(err.Error())
				} else {
					err = t.Execute(w, &reservations)
					if err != nil {
						l.err(err.Error())
					}
				}

			}

		}
	})

	http.HandleFunc("/badguests", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			reservations, err := ge.getBadGuests()

			if err != nil {
				l.err(err.Error())
				r := Response{"success": false, "message": "Unable to server views"}
				writeResponse(w, r)
			} else {
				t, err := template.ParseFiles(htmlPath + "/public/badguests.html")
				if err != nil {
					l.err(err.Error())
				} else {
					err = t.Execute(w, &reservations)
					if err != nil {
						l.err(err.Error())
					}
				}
			}
		}
	})

	http.HandleFunc("/nodes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			reservations, err := ge.getNodes()

			if err != nil {
				l.err(err.Error())
				r := Response{"success": false, "message": "Unable to server views"}
				writeResponse(w, r)
			} else {
				t, err := template.ParseFiles(htmlPath + "/public/nodes.html")
				if err != nil {
					l.err(err.Error())
				} else {
					err = t.Execute(w, &reservations)
					if err != nil {
						l.err(err.Error())
					}
				}
			}
		}
	})

	http.HandleFunc("/reservation", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			ge.listReservations(w, r)
			return
		} else if r.Method == "POST" {
			ge.makeReservation(w, r)
			return
		}
		writeError(w, fmt.Sprintf("Invalid method %s", r.Method))
		return
	})
	http.HandleFunc("/checkin", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			ge.doCheckin(w, r)
			return
		}
		writeError(w, fmt.Sprintf("Invalid method %s", r.Method))
		return
	})
	http.HandleFunc("/checkout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			ge.doCheckOut(w, r)
			return
		}
		writeError(w, fmt.Sprintf("Invalid method %s", r.Method))
		return
	})
	http.HandleFunc("/snooze", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			ge.doSnooze(w, r)
			return
		}
		writeError(w, fmt.Sprintf("Invalid method %s", r.Method))
		return
	})
	http.HandleFunc("/is-coordinator", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			ge.isCoordinator(w, r)
			return
		}
		writeError(w, fmt.Sprintf("Invalid method %s", r.Method))
		return
	})

	server := http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
	log.Panic(server)
}
