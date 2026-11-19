# Backup Manager as a container.
#   scripts/build.sh && docker build -t backup-manager .
# The program is built outside (one static file) and copied in, so the image needs no compiler.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 870 --home-dir /data --shell /usr/sbin/nologin backupmgr && install -d -m 0700 -o backupmgr -g backupmgr /data
COPY bin/backup-manager /usr/local/bin/backup-manager
ENV BM_DATA_DIR=/data BM_MASTER_KEY_FILE=/data/keys/master.key BM_LISTEN=0.0.0.0:8750
USER backupmgr
VOLUME /data
EXPOSE 8750
HEALTHCHECK --interval=30s --timeout=5s CMD ["/usr/local/bin/backup-manager", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/backup-manager"]
CMD ["serve"]
