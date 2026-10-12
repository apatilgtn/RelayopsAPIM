package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/relayops/apim/internal/store"
)

// The archive deliberately omits request payloads, credentials and before/after
// configuration. Identity and action metadata remain for compliance review.
type archivedAudit struct {
	ID           int64     `json:"id"`
	TS           time.Time `json:"ts"`
	Actor        string    `json:"actor"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	TenantID     string    `json:"tenant_id"`
}

func (s *Server) exportAuditArchive(w http.ResponseWriter, r *http.Request) {
	if !platformReleaseAdmin(r) {
		writeErr(w, 403, "platform_only", "audit archival requires a platform administrator")
		return
	}
	before := time.Now().UTC()
	if raw := r.URL.Query().Get("before"); raw != "" {
		var err error
		before, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			writeErr(w, 400, "invalid_before", "use an RFC3339 timestamp")
			return
		}
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT id,ts,actor,action,resource_type,resource_id,tenant_id FROM audit_logs WHERE ts<$1 ORDER BY id LIMIT 10000`, before)
	if err != nil {
		s.fail(w, err)
		return
	}
	records := []archivedAudit{}
	ids := []int64{}
	for rows.Next() {
		var x archivedAudit
		if err = rows.Scan(&x.ID, &x.TS, &x.Actor, &x.Action, &x.ResourceType, &x.ResourceID, &x.TenantID); err != nil {
			rows.Close()
			s.fail(w, err)
			return
		}
		records = append(records, x)
		ids = append(ids, x.ID)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		s.fail(w, err)
		return
	}
	raw, err := json.Marshal(records)
	if err != nil {
		s.fail(w, err)
		return
	}
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	var id string
	if err = s.store.Pool.QueryRow(r.Context(), `INSERT INTO audit_archives(digest,record_ids,created_by) VALUES($1,$2,$3) RETURNING id`, digest, ids, s.getActor(r)).Scan(&id); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "AUDIT_ARCHIVE_EXPORT", "audit_archive", id, map[string]any{"digest": digest, "records": len(records)})
	w.Header().Set("Content-Disposition", `attachment; filename="relayops-audit-archive.json"`)
	writeJSON(w, 200, map[string]any{"archive_id": id, "digest": digest, "records": records, "redacted": true, "limit": 10000, "may_have_more": len(records) == 10000})
}
func (s *Server) purgeAuditArchive(w http.ResponseWriter, r *http.Request) {
	u, _ := r.Context().Value(userCtxKey{}).(store.AdminUser)
	if !scopeFrom(r).Platform || u.Role != "superadmin" {
		writeErr(w, 403, "platform_only", "audit retention deletion requires a platform superadministrator")
		return
	}
	days, err := strconv.Atoi(os.Getenv("RELAYOPS_AUDIT_RETENTION_DAYS"))
	if err != nil || days < 1 {
		writeErr(w, 412, "retention_disabled", "audit records are preserved; explicitly configure RELAYOPS_AUDIT_RETENTION_DAYS before deletion")
		return
	}
	var in struct {
		Digest   string `json:"digest"`
		Archived bool   `json:"archived"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Archived || in.Digest == "" {
		writeErr(w, 412, "archive_confirmation_required", "store and verify the downloaded archive, then confirm its digest and archived=true")
		return
	}
	var deleted int64
	err = s.store.WithTx(r.Context(), func(tx pgx.Tx) error {
		var digest string
		var ids []int64
		var purged *time.Time
		if err := tx.QueryRow(r.Context(), `SELECT digest,record_ids,purged_at FROM audit_archives WHERE id=$1 FOR UPDATE`, r.PathValue("id")).Scan(&digest, &ids, &purged); err != nil {
			return err
		}
		if digest != in.Digest {
			return errors.New("archive digest mismatch")
		}
		if purged != nil {
			return nil
		}
		cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		var tooNew int
		if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM audit_logs WHERE id=ANY($1) AND ts >= $2`, ids, cutoff).Scan(&tooNew); err != nil {
			return err
		}
		if tooNew > 0 {
			return errors.New("archive includes records still within retention")
		}
		tag, err := tx.Exec(r.Context(), `DELETE FROM audit_logs WHERE id=ANY($1) AND ts<$2`, ids, cutoff)
		if err != nil {
			return err
		}
		deleted = tag.RowsAffected()
		// Record the destructive action in the same transaction; it is not in the archive's IDs.
		if _, err = tx.Exec(r.Context(), `INSERT INTO audit_logs(actor,action,resource_type,resource_id,details) VALUES($1,'AUDIT_ARCHIVE_PURGE','audit_archive',$2,$3)`, s.getActor(r), r.PathValue("id"), map[string]any{"records": deleted, "digest": digest, "retention_days": days}); err != nil {
			return err
		}
		_, err = tx.Exec(r.Context(), `UPDATE audit_archives SET purged_at=now() WHERE id=$1`, r.PathValue("id"))
		return err
	})
	if err != nil {
		writeErr(w, 412, "retention_blocked", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": deleted})
}
