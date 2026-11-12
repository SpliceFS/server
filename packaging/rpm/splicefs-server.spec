# rpmbuild only wraps files here. scripts/package-rpm.sh passes in where the compiled
# program (stage), the service unit (unit) and the source tree (src) are.
Name:           splicefs-server
Version:        %{pkg_version}
Release:        1
Summary:        Server of the SpliceFS read-only multi-source filesystem
License:        MIT
URL:            https://github.com/SpliceFS/server
# Files, not a package name: the tools come from shadow-utils on Red Hat and from shadow
# on SUSE.
Requires(pre):  /usr/sbin/useradd
Requires(pre):  /usr/sbin/groupadd

# The binary is static and already stripped, and it may be for another CPU type than the
# one building the package, so the usual post-processing has nothing it could do right.
%global debug_package %{nil}
%global __os_install_post %{nil}
%global _build_id_links none
# gzip is the payload format every rpm version in use can read.
%global _binary_payload w9.gzdio

%description
splicefsd serves local directories over HTTP so that SpliceFS clients can read
each file from every node that holds a copy, at the combined speed of those
nodes. It only reads; it never writes to a shared directory.

%install
install -D -m 755 %{stage}/splicefsd %{buildroot}/usr/bin/splicefsd
install -D -m 644 %{unit} %{buildroot}/usr/lib/systemd/system/splicefsd.service
install -d %{buildroot}/etc/splicefs/server
install -D -m 644 %{src}/LICENSE %{buildroot}/usr/share/doc/splicefs-server/LICENSE
install -m 644 %{src}/README.md %{src}/CHANGELOG.md %{src}/docs/configuration.md \
    %{buildroot}/usr/share/doc/splicefs-server/

%files
/usr/bin/splicefsd
/usr/lib/systemd/system/splicefsd.service
%dir /etc/splicefs
%dir /etc/splicefs/server
/usr/share/doc/splicefs-server

%pre
# The server runs as its own user so that a bug in it has no more rights than reading
# what that user was given.
getent group splicefs >/dev/null || groupadd -r splicefs
getent passwd splicefs >/dev/null || useradd -r -g splicefs -M -d / -s /sbin/nologin splicefs

%post
# $1 is 1 on a first install and 2 on an upgrade. A container or a chroot has no running
# systemd; the files are installed and there is nothing to start.
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload
    if [ "$1" -eq 1 ]; then
        systemctl enable splicefsd.service
        systemctl start splicefsd.service ||
            echo "splicefsd did not start; see: journalctl -u splicefsd" >&2
    else
        # A broken configuration must not turn an upgrade into a failed transaction.
        systemctl try-restart splicefsd.service ||
            echo "splicefsd did not restart; see: journalctl -u splicefsd" >&2
    fi
fi

%preun
# $1 is 0 when the package is removed for good, not replaced by another version.
if [ "$1" -eq 0 ] && [ -d /run/systemd/system ]; then
    systemctl disable --now splicefsd.service || true
fi

%postun
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
