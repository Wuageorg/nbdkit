# THIS FILE WAS AUTO-GENERATED
#
#  $ lcitool manifest ci/manifest.yml
#
# https://gitlab.com/libvirt/libvirt-ci

function install_buildenv() {
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get dist-upgrade -y
    apt-get install --no-install-recommends -y \
            autoconf \
            automake \
            bash \
            bash-completion \
            bsdextrautils \
            ca-certificates \
            cargo \
            ccache \
            clang \
            e2fsprogs \
            expect \
            fdisk \
            flake8 \
            g++ \
            gcc \
            genisoimage \
            git \
            golang \
            gzip \
            iproute2 \
            jq \
            libcurl4-gnutls-dev \
            libgnutls28-dev \
            libguestfs-dev \
            liblzma-dev \
            libperl-dev \
            libselinux1-dev \
            libssh-gcrypt-dev \
            libtool-bin \
            libtorrent-dev \
            libvirt-dev \
            libzstd-dev \
            locales \
            lua5.4 \
            make \
            mount \
            ocaml \
            original-awk \
            perl \
            perl-base \
            pkgconf \
            python3 \
            python3-boto3 \
            python3-dev \
            python3-libnbd \
            qemu-utils \
            rustc \
            socat \
            tcl-dev \
            xz-utils \
            zlib1g-dev
    sed -Ei 's,^# (en_US\.UTF-8 .*)$,\1,' /etc/locale.gen
    dpkg-reconfigure locales
    dpkg-query --showformat '${Package}_${Version}_${Architecture}\n' --show > /packages.txt
    mkdir -p /usr/libexec/ccache-wrappers
    ln -s /usr/bin/ccache /usr/libexec/ccache-wrappers/c++
    ln -s /usr/bin/ccache /usr/libexec/ccache-wrappers/cc
    ln -s /usr/bin/ccache /usr/libexec/ccache-wrappers/clang
    ln -s /usr/bin/ccache /usr/libexec/ccache-wrappers/g++
    ln -s /usr/bin/ccache /usr/libexec/ccache-wrappers/gcc
}

export CCACHE_WRAPPERSDIR="/usr/libexec/ccache-wrappers"
export LANG="en_US.UTF-8"
export MAKE="/usr/bin/make"
export PYTHON="/usr/bin/python3"
