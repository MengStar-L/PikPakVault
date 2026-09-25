package vault

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func rssBackupFixture(t *testing.T, s *Store) string {
	t.Helper()
	secret, e := s.Seal(map[string]string{"url": "https://feeds.example.test/private.xml?token=fixture-rss-token"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(`INSERT INTO rss_subscriptions(id,name,secret,parent_id,interval_minutes,enabled,import_existing,initialized,last_checked,next_check,last_error,etag,last_modified,created)
		VALUES('rss-fixture','Private feed',?,'root',45,1,0,1,1700000000,1700002700,'','fixture-etag','Tue, 14 Nov 2023 22:13:20 GMT',1699999999)`, secret); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(`INSERT INTO rss_entries(id,subscription_id,entry_key,resource_key,title,published,discovered,state,message,job_id,account_id,source_id) VALUES
		('rss-seen','rss-fixture','seen-guid','seen-resource','Already seen',1699900000,1700000000,'skipped','Initial baseline','','',''),
		('rss-imported','rss-fixture','imported-guid','magnet:fixture','Saved movie.mp4',1699990000,1700000000,'pending','Queued','rss-job','a','rss-source')`); e != nil {
		t.Fatal(e)
	}
	return secret
}

func rssBackupSnapshot(t *testing.T, s *Store) string {
	t.Helper()
	// Include scheduling, destination and identity fields so imports cannot
	// silently reset the initial baseline or re-enqueue previously seen entries.
	var snapshot string
	e := s.DB.QueryRow(`SELECT json_object(
		'subscriptions',(SELECT json_group_array(json_object('id',id,'name',name,'parent_id',parent_id,'interval_minutes',interval_minutes,'enabled',enabled,'import_existing',import_existing,'initialized',initialized,'last_checked',last_checked,'next_check',next_check,'last_error',last_error,'etag',etag,'last_modified',last_modified,'created',created)) FROM (SELECT * FROM rss_subscriptions ORDER BY id)),
		'entries',(SELECT json_group_array(json_object('id',id,'subscription_id',subscription_id,'entry_key',entry_key,'resource_key',resource_key,'title',title,'published',published,'discovered',discovered,'state',state,'message',message,'job_id',job_id,'account_id',account_id,'source_id',source_id)) FROM (SELECT * FROM rss_entries ORDER BY id)))`).Scan(&snapshot)
	if e != nil {
		t.Fatal(e)
	}
	return snapshot
}

func TestRSSBackupPreservesSecretsAndDedupe(t *testing.T) {
	original, _ := testApp(t)
	ciphertext := rssBackupFixture(t, original.Store)
	expected := rssBackupSnapshot(t, original.Store)
	archive := filepath.Join(t.TempDir(), "rss.zip")
	if e := original.Store.Backup(archive); e != nil {
		t.Fatal(e)
	}
	src, e := ExtractBackup(archive, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer src.DB.Close()
	dst, _ := testApp(t)
	if e = replaceData(dst.Store, src); e != nil {
		t.Fatal(e)
	}
	if got := rssBackupSnapshot(t, dst.Store); got != expected {
		t.Fatalf("RSS metadata changed during import:\ngot %s\nwant %s", got, expected)
	}
	var rewrapped string
	if e = dst.Store.DB.QueryRow(`SELECT secret FROM rss_subscriptions WHERE id='rss-fixture'`).Scan(&rewrapped); e != nil {
		t.Fatal(e)
	}
	if rewrapped == ciphertext || strings.Contains(rewrapped, "fixture-rss-token") {
		t.Fatal("RSS authentication URL was not encrypted with the destination key")
	}
	var credentials map[string]string
	if e = dst.Store.Unseal(rewrapped, &credentials); e != nil || credentials["url"] != "https://feeds.example.test/private.xml?token=fixture-rss-token" {
		t.Fatal("RSS authentication URL was lost during import", e)
	}
	if dst.Store.Get("import_review_required") != "true" {
		t.Fatal("imported automatic subscriptions must wait for administrator review")
	}
	if _, e = dst.Store.DB.Exec(`INSERT INTO rss_entries(id,subscription_id,entry_key,title,discovered,state) VALUES('duplicate','rss-fixture','seen-guid','Seen again',1700010000,'new')`); e == nil {
		t.Fatal("imported entry identity allowed a duplicate")
	}
}

func TestRSSImportLegacyBackupClearsSubscriptions(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("schema%d", version), func(t *testing.T) {
			original, _ := testApp(t)
			if _, e := original.Store.DB.Exec(`DROP TABLE rss_entries; DROP TABLE rss_subscriptions`); e != nil {
				t.Fatal(e)
			}
			if version == 1 {
				if _, e := original.Store.DB.Exec(`DROP TABLE teldrive_monitors`); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := original.Store.DB.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, version)); e != nil {
				t.Fatal(e)
			}
			archive := filepath.Join(t.TempDir(), "legacy.zip")
			if e := original.Store.Backup(archive); e != nil {
				t.Fatal(e)
			}
			src, e := ExtractBackup(archive, t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			defer src.DB.Close()
			dst, _ := testApp(t)
			rssBackupFixture(t, dst.Store)
			if e = replaceData(dst.Store, src); e != nil {
				t.Fatal(e)
			}
			for _, table := range []string{"rss_subscriptions", "rss_entries"} {
				var count int
				if e = dst.Store.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); e != nil || count != 0 {
					t.Fatalf("legacy import kept previous %s: count=%d error=%v", table, count, e)
				}
			}
			var importedVersion int
			if e = dst.Store.DB.QueryRow(`PRAGMA user_version`).Scan(&importedVersion); e != nil || importedVersion != 3 {
				t.Fatalf("legacy import changed current schema: version=%d error=%v", importedVersion, e)
			}
			if dst.Store.Get("import_review_required") != "true" {
				t.Fatal("legacy import did not pause background scheduling")
			}
		})
	}
}

func TestRSSBackupRejectsFutureSchemaAndMismatchedCredentials(t *testing.T) {
	t.Run("future-schema", func(t *testing.T) {
		a, _ := testApp(t)
		if _, e := a.Store.DB.Exec(`PRAGMA user_version=4`); e != nil {
			t.Fatal(e)
		}
		if e := validateBackup(a.Store); e == nil || !strings.Contains(e.Error(), "数据库版本") {
			t.Fatal("unsupported future schema was not rejected", e)
		}
	})
	t.Run("rss-credentials", func(t *testing.T) {
		a, _ := testApp(t)
		rssBackupFixture(t, a.Store)
		other, _ := testApp(t)
		foreign, e := other.Store.Seal(map[string]string{"url": "https://feeds.example.test/private.xml?token=other"})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = a.Store.DB.Exec(`UPDATE rss_subscriptions SET secret=?`, foreign); e != nil {
			t.Fatal(e)
		}
		if e = validateBackup(a.Store); e == nil || !strings.Contains(e.Error(), "rss_subscriptions") {
			t.Fatal("RSS secret encrypted with a different key was accepted", e)
		}
	})
}
