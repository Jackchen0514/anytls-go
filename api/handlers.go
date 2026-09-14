package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"anytls/user"
	"anytls/util"
)

// releaseRepo is the GitHub repo release binaries/tags are published under,
// used to check for a newer version from /api/version.
const releaseRepo = "Jackchen0514/anytls-go"

type versionResponse struct {
	Version         string `json:"version"`
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"update_available,omitempty"`
	CheckError      string `json:"check_error,omitempty"`
}

// handleVersion reports the running binary's version and, best-effort,
// the latest version published on GitHub (never fatal if that check fails,
// e.g. no outbound internet access or GitHub rate limiting).
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	resp := versionResponse{Version: util.ProgramVersion}
	latest, err := latestReleaseVersion(r.Context())
	if err != nil {
		resp.CheckError = err.Error()
	} else {
		resp.Latest = latest
		resp.UpdateAvailable = latest != resp.Version
	}
	writeJSON(w, http.StatusOK, resp)
}

func latestReleaseVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/"+releaseRepo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github api returned status %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return strings.TrimPrefix(body.TagName, "v"), nil
}

type userView struct {
	*user.User
	Usage user.Usage `json:"usage"`
}

func (s *Server) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"listen": s.serverAddr})
}

func (s *Server) view(u *user.User) userView {
	usage, _ := s.manager.GetUsage(u.ID)
	return userView{User: u, Usage: usage}
}

type userRequest struct {
	Username          *string  `json:"username"`
	Password          *string  `json:"password"`
	Enabled           *bool    `json:"enabled"`
	TrafficLimitBytes *int64   `json:"traffic_limit_bytes"`
	TrafficMultiplier *float64 `json:"traffic_multiplier"`
	IPLimit           *int     `json:"ip_limit"`
	ConnLimit         *int     `json:"conn_limit"`
	TrafficResetCycle *string  `json:"traffic_reset_cycle"`
	// ExpiresAt is RFC3339 (e.g. "2026-12-31T23:59:59Z"), or "" to clear
	// (never expires).
	ExpiresAt *string `json:"expires_at"`
}

func validResetCycle(c string) bool {
	switch user.ResetCycle(c) {
	case user.ResetCycleNone, user.ResetCycleDaily, user.ResetCycleMonthly:
		return true
	default:
		return false
	}
}

// parseExpiresAt parses an RFC3339 timestamp, treating "" as "never expires".
func parseExpiresAt(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		users, err := s.manager.ListUsers()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		views := make([]userView, 0, len(users))
		for _, u := range users {
			views = append(views, s.view(u))
		}
		writeJSON(w, http.StatusOK, views)

	case http.MethodPost:
		var req userRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.Username == nil || *req.Username == "" || req.Password == nil || *req.Password == "" {
			writeError(w, http.StatusBadRequest, "username and password are required")
			return
		}
		if req.TrafficResetCycle != nil && !validResetCycle(*req.TrafficResetCycle) {
			writeError(w, http.StatusBadRequest, "traffic_reset_cycle must be one of none|daily|monthly")
			return
		}
		if req.TrafficMultiplier != nil && *req.TrafficMultiplier <= 0 {
			writeError(w, http.StatusBadRequest, "traffic_multiplier must be greater than 0")
			return
		}
		var expiresAt time.Time
		if req.ExpiresAt != nil {
			var err error
			expiresAt, err = parseExpiresAt(*req.ExpiresAt)
			if err != nil {
				writeError(w, http.StatusBadRequest, "expires_at must be RFC3339, e.g. 2026-12-31T23:59:59Z")
				return
			}
		}

		u := &user.User{
			Username:          *req.Username,
			Password:          *req.Password,
			Enabled:           true,
			TrafficResetCycle: user.ResetCycleNone,
			ExpiresAt:         expiresAt,
		}
		if req.Enabled != nil {
			u.Enabled = *req.Enabled
		}
		if req.TrafficLimitBytes != nil {
			u.TrafficLimitBytes = *req.TrafficLimitBytes
		}
		if req.TrafficMultiplier != nil {
			u.TrafficMultiplier = *req.TrafficMultiplier
		}
		if req.IPLimit != nil {
			u.IPLimit = *req.IPLimit
		}
		if req.ConnLimit != nil {
			u.ConnLimit = *req.ConnLimit
		}
		if req.TrafficResetCycle != nil {
			u.TrafficResetCycle = user.ResetCycle(*req.TrafficResetCycle)
		}

		created, err := s.manager.CreateUser(u)
		if err != nil {
			if errors.Is(err, user.ErrDuplicateUsername) {
				writeError(w, http.StatusConflict, "username already exists")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, s.view(created))

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleUserByID routes:
//
//	GET/PUT/DELETE /api/users/{id}
//	POST           /api/users/{id}/reset-traffic
func (s *Server) handleUserByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/users/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	if len(parts) == 2 && parts[1] == "reset-traffic" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if err := s.manager.ResetTraffic(id); err != nil {
			respondStoreErr(w, err)
			return
		}
		u, err := s.manager.GetUser(id)
		if err != nil {
			respondStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.view(u))
		return
	}
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		u, err := s.manager.GetUser(id)
		if err != nil {
			respondStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.view(u))

	case http.MethodPut:
		u, err := s.manager.GetUser(id)
		if err != nil {
			respondStoreErr(w, err)
			return
		}
		var req userRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.TrafficResetCycle != nil && !validResetCycle(*req.TrafficResetCycle) {
			writeError(w, http.StatusBadRequest, "traffic_reset_cycle must be one of none|daily|monthly")
			return
		}
		if req.TrafficMultiplier != nil && *req.TrafficMultiplier <= 0 {
			writeError(w, http.StatusBadRequest, "traffic_multiplier must be greater than 0")
			return
		}
		if req.ExpiresAt != nil {
			expiresAt, err := parseExpiresAt(*req.ExpiresAt)
			if err != nil {
				writeError(w, http.StatusBadRequest, "expires_at must be RFC3339, e.g. 2026-12-31T23:59:59Z")
				return
			}
			u.ExpiresAt = expiresAt
		}
		if req.Username != nil {
			u.Username = *req.Username
		}
		if req.Password != nil {
			u.Password = *req.Password
		}
		if req.Enabled != nil {
			u.Enabled = *req.Enabled
		}
		if req.TrafficLimitBytes != nil {
			u.TrafficLimitBytes = *req.TrafficLimitBytes
		}
		if req.TrafficMultiplier != nil {
			u.TrafficMultiplier = *req.TrafficMultiplier
		}
		if req.IPLimit != nil {
			u.IPLimit = *req.IPLimit
		}
		if req.ConnLimit != nil {
			u.ConnLimit = *req.ConnLimit
		}
		if req.TrafficResetCycle != nil {
			u.TrafficResetCycle = user.ResetCycle(*req.TrafficResetCycle)
		}
		if err := s.manager.UpdateUser(u); err != nil {
			respondStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.view(u))

	case http.MethodDelete:
		if err := s.manager.DeleteUser(id); err != nil {
			respondStoreErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func respondStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, user.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if errors.Is(err, user.ErrDuplicateUsername) {
		writeError(w, http.StatusConflict, "username already exists")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
