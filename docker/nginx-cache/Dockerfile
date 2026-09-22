FROM nginx:1.31-alpine

# Add curl for healthchecks (small addition)
RUN apk add --no-cache curl

# Default nginx config (can be overridden via volume mount)
COPY nginx.conf /etc/nginx/nginx.conf

# Prepare cache + temp directories with correct permissions (nginx user in the alpine image).
# A mounted volume hides these, so 40-prepare-cache-dirs.sh recreates them at start.
RUN mkdir -p /var/cache/nginx/cache /var/cache/nginx/temp && \
    chown -R nginx:nginx /var/cache/nginx /var/log/nginx

COPY 40-prepare-cache-dirs.sh /docker-entrypoint.d/40-prepare-cache-dirs.sh
RUN chmod 755 /docker-entrypoint.d/40-prepare-cache-dirs.sh

EXPOSE 8080

CMD ["nginx", "-g", "daemon off;"]
