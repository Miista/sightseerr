"""Build the minimal extract `sightseerr backfill` reads, from a live jellyfin.db.

Run on the Jellyfin host, never elsewhere: jellyfin.db holds password hashes.
It takes a private snapshot with SQLite's backup API (read-only on the live
file), copies out only the played PLACEHOLDER rows (user, last played, the
provider key) into a fresh database, and deletes the snapshot.

    python3 backfill-extract.py <jellyfin.db> <extract.db> <scratch dir>
"""
import os
import sqlite3
import sys

src, dst, scratch = sys.argv[1:4]
snap = os.path.join(scratch, "jellyfin-snapshot.db")
try:
    live = sqlite3.connect(f"file:{src}?mode=ro", uri=True)
    copy = sqlite3.connect(snap)
    live.backup(copy)
    live.close()
    rows = copy.execute(
        """SELECT u.UserId, u.LastPlayedDate, u.CustomDataKey, u.Played, u.ItemId
           FROM UserData u JOIN BaseItems b ON b.Id = u.ItemId
           WHERE b.Type = 'PLACEHOLDER' AND u.Played = 1"""
    ).fetchall()
    copy.close()
finally:
    if os.path.exists(snap):
        os.remove(snap)

if os.path.exists(dst):
    sys.exit(f"{dst} already exists")
out = sqlite3.connect(dst)
out.execute("CREATE TABLE BaseItems (Id TEXT PRIMARY KEY, Type TEXT)")
out.execute("CREATE TABLE UserData (UserId TEXT, LastPlayedDate TEXT, CustomDataKey TEXT, Played INTEGER, ItemId TEXT)")
out.executemany("INSERT OR IGNORE INTO BaseItems VALUES (?, 'PLACEHOLDER')", {(r[4],) for r in rows})
out.executemany("INSERT INTO UserData VALUES (?, ?, ?, ?, ?)", rows)
out.commit()
out.close()
print(f"{len(rows)} placeholder play rows extracted")
