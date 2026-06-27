# Binary RPM for mailbox. The binary is built outside rpmbuild (the Makefile
# `build` target runs first) and installed from the source tree (srcdir define);
# rpmbuild does not compile Go here. Build with: make rpm
%global debug_package %{nil}
# Releases are tracked as git tags, not an RPM %changelog, so don't derive
# SOURCE_DATE_EPOCH from one (silences a build warning).
%global source_date_epoch_from_changelog 0

Name:           mailbox
Version:        %{?appversion}%{!?appversion:0.0.0}
Release:        1%{?dist}
Summary:        A native, fast Gmail client for GNOME

License:        MIT
URL:            https://github.com/jsnjack/mailbox

Requires:       gtk4
Requires:       libadwaita
Requires:       webkitgtk6.0
Requires:       libsecret

%description
mailbox is a native GTK4/libadwaita desktop email client for Gmail. It presents
a responsive 3-pane layout backed by a local SQLite cache, renders messages in a
locked-down WebKitGTK view, and includes AI assistance (translate, draft reply).

%install
install -Dm0755 %{srcdir}/bin/mailbox %{buildroot}%{_bindir}/mailbox
install -Dm0644 %{srcdir}/packaging/com.jsnjack.mailbox.desktop %{buildroot}%{_datadir}/applications/com.jsnjack.mailbox.desktop
install -Dm0644 %{srcdir}/packaging/com.jsnjack.mailbox.svg %{buildroot}%{_datadir}/icons/hicolor/scalable/apps/com.jsnjack.mailbox.svg

%files
%{_bindir}/mailbox
%{_datadir}/applications/com.jsnjack.mailbox.desktop
%{_datadir}/icons/hicolor/scalable/apps/com.jsnjack.mailbox.svg
