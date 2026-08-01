#!/bin/sh
# Remap the unprivileged app user onto the host's UID/GID, then drop root.
#
# Synology volume mounts are owned by a specific host user, so a container that
# runs as a fixed UID cannot write to them. PUID/PGID let the operator line the
# container user up with whoever owns the share.

set -eu

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"

# Already unprivileged (docker run --user ...): nothing to remap.
if [ "$(id -u)" != "0" ]; then
    exec "$@"
fi

current_gid="$(getent group app | cut -d: -f3)"
if [ "$current_gid" != "$PGID" ]; then
    groupmod -o -g "$PGID" app
fi

current_uid="$(id -u app)"
if [ "$current_uid" != "$PUID" ]; then
    usermod -o -u "$PUID" app
fi

# Only the mount points are re-owned; a full recursive chown of a large torrent
# directory would make every start slow for no benefit.
for dir in /data "${TORRENT_FILES_DIR:-/torrents}"; do
    [ -d "$dir" ] || mkdir -p "$dir"
    chown "$PUID:$PGID" "$dir"
done

# The database file itself must follow the remap, otherwise a changed PUID
# leaves an unwritable app.db behind.
db_path="${DB_PATH:-/data/app.db}"
for file in "$db_path" "$db_path-wal" "$db_path-shm"; do
    # An if-block, not "[ -e ] && chown": under set -e a false test at the end
    # of an && list would abort the entrypoint on a first run.
    if [ -e "$file" ]; then
        chown "$PUID:$PGID" "$file"
    fi
done

echo "entrypoint: running as ${PUID}:${PGID}"
exec su-exec "$PUID:$PGID" "$@"
