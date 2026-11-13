# Sourced by scripts/freebsd-job.sh inside the FreeBSD machine. It says what "the service
# works" means for this program; the rest of the job is the same for every SpliceFS program.

# The server runs as its own user, which owns nothing but its data folder.
expected_user=splicefs

# Status 0 when the health endpoint answers. The default listener is every interface on
# port 8480, TLS with a certificate the server makes for itself, so the check does not
# verify the certificate.
answers() {
    fetch --no-verify-peer -qo /dev/null https://127.0.0.1:8480/api/v1/health 2>/dev/null
}
