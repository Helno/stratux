#!/bin/bash -e
# Script to configure and build Stratux images with pi-gen

arch=`uname -m`
echo "arch ${arch}"

# disable full image (stage 5), normal image (stage 4) and desktop image (stage 3)
touch pi-gen/stage3/SKIP pi-gen/stage4/SKIP pi-gen/stage5/SKIP

# and disable building the images based on the SKIPPED stages
touch pi-gen/stage3/SKIP_IMAGES pi-gen/stage4/SKIP_IMAGES pi-gen/stage5/SKIP_IMAGES

# copy the config file into place
cp config pi-gen/

# copy the stage2 10-stratux files into place
rsync -av --delete stage2/10-stratux/ pi-gen/stage2/10-stratux/

# copy the local git repository (with full git history) into pi-gen
# so the files and git metadata are visible to docker for version detection
local_git=`pwd`/../

# First, make sure the destination doesn't exist
(cd pi-gen && rm -rf stratux)

# Use tar to preserve git metadata while copying, avoiding circular reference
tar -C ${local_git} --exclude='image_build' -czf /tmp/stratux.tar.gz .
mkdir -p pi-gen/stratux
tar -xzf /tmp/stratux.tar.gz -C pi-gen/stratux
rm /tmp/stratux.tar.gz

(cd pi-gen/stratux && git submodule update --init --recursive 2>/dev/null || echo "Git submodules skipped")
# Build the stratux debian package.
(cd pi-gen/stratux && make ddpkg)
ERRCODE=$?
if [ $ERRCODE -ne 0 ]; then
	echo "Error creating the stratux debian package: Returned $ERRCODE"
	exit $ERRCODE 
fi

# build via docker
#
# pass PRESERVE_CONTAINER=1 to keep the container in the case of error
# to enable debugging
(cd pi-gen && ./build-docker.sh)
