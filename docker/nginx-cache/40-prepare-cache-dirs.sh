#!/bin/sh
# The volume is mounted at /var/cache/nginx and hides the directories from the
# image. Recreate the cache and temp dirs, owned by the nginx worker, before
# nginx scans the cache path. Do not touch lost+found at the volume root.
set -e
mkdir -p /var/cache/nginx/cache /var/cache/nginx/temp
chown -R nginx:nginx /var/cache/nginx/cache /var/cache/nginx/temp
