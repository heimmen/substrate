FROM nginx:latest
# Environment-agnostic placeholder; run-nginx*.sh bind-mounts the real
# per-environment conf (nginx-prod.conf / nginx-qa.conf / nginx-test.conf)
# over this file at runtime.
COPY nginx-default.conf /etc/nginx/conf.d/default.conf
COPY mfpi-admin.htpasswd /etc/nginx/mfpi-admin.htpasswd
