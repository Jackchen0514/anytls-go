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

	"anytls/proxy"
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

// directGroupView is the synthetic row representing user.DirectGroupID,
// prepended to every /api/outbound-groups listing so clients (the web
// panel's dropdown in particular) don't need to special-case "no group."
var directGroupView = &user.OutboundGroup{ID: user.DirectGroupID, Name: "直连"}

type outboundGroupRequest struct {
	Name      *string `json:"name"`
	SOCKS5URL *string `json:"socks5_url"`
}

// handleOutboundGroups routes:
//
//	GET  /api/outbound-groups - list all groups, direct group first
//	POST /api/outbound-groups - create one, fields: name, socks5_url (both required)
func (s *Server) handleOutboundGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		groups, err := s.manager.ListOutboundGroups()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		views := make([]*user.OutboundGroup, 0, len(groups)+1)
		views = append(views, directGroupView)
		views = append(views, groups...)
		writeJSON(w, http.StatusOK, views)

	case http.MethodPost:
		var req outboundGroupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.Name == nil || *req.Name == "" || req.SOCKS5URL == nil || *req.SOCKS5URL == "" {
			writeError(w, http.StatusBadRequest, "name and socks5_url are required")
			return
		}
		created, err := s.manager.CreateOutboundGroup(&user.OutboundGroup{Name: *req.Name, SOCKS5URL: *req.SOCKS5URL})
		if err != nil {
			respondGroupErr(w, err)
			return
		}
		if err := proxy.SetGroupSOCKS5(created.ID, created.SOCKS5URL); err != nil {
			// socks5_url is already persisted at this point; if it somehow
			// fails to apply live, the group still loads correctly (direct
			// fallback) and can be fixed with a PUT, so this isn't fatal to
			// the request - but it would be surprising for the caller not
			// to know, so report it rather than silently returning 201.
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, created)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleOutboundGroupByID routes:
//
//	PUT    /api/outbound-groups/{id} - update name and/or socks5_url
//	DELETE /api/outbound-groups/{id} - delete; any user assigned to it
//	                                    reverts to the direct group
func (s *Server) handleOutboundGroupByID(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/outbound-groups/")
	id, err := strconv.ParseInt(strings.Trim(idStr, "/"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid group id")
		return
	}
	if id == user.DirectGroupID {
		writeError(w, http.StatusBadRequest, "the direct group is built-in and can't be modified")
		return
	}

	switch r.Method {
	case http.MethodPut:
		g, err := s.manager.GetOutboundGroup(id)
		if err != nil {
			respondGroupErr(w, err)
			return
		}
		var req outboundGroupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.Name != nil {
			g.Name = *req.Name
		}
		if req.SOCKS5URL != nil {
			g.SOCKS5URL = *req.SOCKS5URL
		}
		if g.Name == "" || g.SOCKS5URL == "" {
			writeError(w, http.StatusBadRequest, "name and socks5_url are required")
			return
		}
		if err := s.manager.UpdateOutboundGroup(g); err != nil {
			respondGroupErr(w, err)
			return
		}
		if err := proxy.SetGroupSOCKS5(g.ID, g.SOCKS5URL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, g)

	case http.MethodDelete:
		if err := s.manager.DeleteOutboundGroup(id); err != nil {
			respondGroupErr(w, err)
			return
		}
		proxy.RemoveGroup(id)
		w.WriteHeader(http.StatusNoContent)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func respondGroupErr(w http.ResponseWriter, err error) {
	if errors.Is(err, user.ErrGroupNotFound) {
		writeError(w, http.StatusNotFound, "outbound group not found")
		return
	}
	if errors.Is(err, user.ErrDuplicateGroupName) {
		writeError(w, http.StatusConflict, "outbound group name already exists")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
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
	// OutboundGroupID selects which outbound this user's traffic dials
	// through: user.DirectGroupID (0) or an existing OutboundGroup's ID.
	OutboundGroupID *int64 `json:"outbound_group_id"`
}

// validOutboundGroup reports whether id is the direct group or an existing
// OutboundGroup.
func (s *Server) validOutboundGroup(id int64) bool {
	if id == user.DirectGroupID {
		return true
	}
	_, err := s.manager.GetOutboundGroup(id)
	return err == nil
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
		if req.OutboundGroupID != nil && !s.validOutboundGroup(*req.OutboundGroupID) {
			writeError(w, http.StatusBadRequest, "outbound_group_id must be 0 (direct) or an existing outbound group id")
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
		if req.OutboundGroupID != nil {
			u.OutboundGroupID = *req.OutboundGroupID
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
//	GET/PUT/DELETE          /api/users/{id}
//	POST                    /api/users/{id}/reset-traffic
//	GET/POST/DELETE (body)  /api/users/{id}/blocked-domains
//	GET/DELETE              /api/users/{id}/visit-history
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

	if len(parts) == 2 && parts[1] == "blocked-domains" {
		s.handleBlockedDomains(w, r, id)
		return
	}

	if len(parts) == 2 && parts[1] == "visit-history" {
		s.handleVisitHistory(w, r, id)
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
		if req.OutboundGroupID != nil && !s.validOutboundGroup(*req.OutboundGroupID) {
			writeError(w, http.StatusBadRequest, "outbound_group_id must be 0 (direct) or an existing outbound group id")
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
		if req.OutboundGroupID != nil {
			u.OutboundGroupID = *req.OutboundGroupID
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

type blockedDomainRequest struct {
	Pattern string `json:"pattern"`
}

// handleBlockedDomains manages a single user's blocklist (domains,
// wildcard domains, exact IPs, and CIDR ranges):
//
//	GET    -> list current patterns
//	POST   -> add one, body {"pattern": "example.com" | "*.example.com" |
//	          "203.0.113.1" | "10.0.0.0/8"}
//	DELETE -> remove one, same body shape (DELETE-with-body rather than a
//	          path segment, since "*" and "/" in a URL path are awkward to
//	          round-trip reliably across HTTP clients)
func (s *Server) handleBlockedDomains(w http.ResponseWriter, r *http.Request, id int64) {
	switch r.Method {
	case http.MethodGet:
		patterns, err := s.manager.ListBlockedDomains(id)
		if err != nil {
			respondStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, patterns)

	case http.MethodPost, http.MethodDelete:
		var req blockedDomainRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		pattern := user.NormalizeBlockPattern(req.Pattern)

		if r.Method == http.MethodPost {
			if !user.ValidBlockPattern(pattern) {
				writeError(w, http.StatusBadRequest, "pattern must be a domain (example.com), a wildcard (*.example.com), an IP (203.0.113.1), or a CIDR range (10.0.0.0/8)")
				return
			}
			if err := s.manager.AddBlockedDomain(id, pattern); err != nil {
				respondStoreErr(w, err)
				return
			}
		} else {
			if err := s.manager.RemoveBlockedDomain(id, pattern); err != nil {
				respondStoreErr(w, err)
				return
			}
		}

		u, err := s.manager.GetUser(id)
		if err != nil {
			respondStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.view(u))

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleVisitHistory manages a single user's recent-visit history (recent
// deduplicated domains/IPs they've been proxied to):
//
//	GET    -> list entries {host, last_visit_at}, most-recent first
//	DELETE -> clear history
func (s *Server) handleVisitHistory(w http.ResponseWriter, r *http.Request, id int64) {
	switch r.Method {
	case http.MethodGet:
		history, ok := s.manager.GetVisitHistory(id)
		if !ok {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		writeJSON(w, http.StatusOK, history)

	case http.MethodDelete:
		if err := s.manager.ClearVisitHistory(id); err != nil {
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
