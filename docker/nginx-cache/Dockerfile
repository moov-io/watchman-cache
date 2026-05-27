FROM nginx:1.27-alpine

# Add curl for healthchecks (small addition)
RUN apk add --no-cache curl

# Default nginx config (can be overridden via volume mount)
COPY nginx.conf /etc/nginx/nginx.conf

# Prepare cache + temp directories with correct permissions (nginx user in the alpine image)
RUN mkdir -p /var/cache/nginx /var/cache/nginx/temp && \
    chown -R nginx:nginx /var/cache/nginx /var/log/nginx

EXPOSE 8080

CMD ["nginx", "-g", "daemon off;"]
