#!/bin/sh
set -e # Have script exit in the event of a failed command.

# connectToMinio
# Use a check-sleep-check loop to wait for Minio service to be available
connectToMinio() {
  SCHEME=$1
  ATTEMPTS=0
  LIMIT=29 # Allow 30 attempts
  set -e   # fail if we can't read the keys.
  ACCESS=$(cat /opt/minio/credentials/accesskey)
  SECRET=$(cat /opt/minio/credentials/secretkey)
  set +e # The connections to minio are allowed to fail.
  echo "Connecting to Minio server: $SCHEME://$MINIO_ENDPOINT:$MINIO_PORT"
  MC_COMMAND="mc config host add myminio $SCHEME://$MINIO_ENDPOINT:$MINIO_PORT $ACCESS $SECRET"
  $MC_COMMAND
  STATUS=$?
  until [ $STATUS = 0 ]; do
    ATTEMPTS=$(expr $ATTEMPTS + 1)
    echo \"Failed attempts: $ATTEMPTS\"
    if [ $ATTEMPTS -gt $LIMIT ]; then
      exit 1
    fi
    sleep 2 # 1 second intervals between attempts
    $MC_COMMAND
    STATUS=$?
  done
  set -e # reset `e` as active
  return 0
}

# checkBucketExists ($bucket)
# Check if the bucket exists, by using the exit code of `mc ls`
checkBucketExists() {
  BUCKET=$1
  CMD=$(/usr/bin/mc ls myminio/$BUCKET >/dev/null 2>&1)
  return $?
}

# createBucket ($bucket, $policy, $purge)
# Ensure bucket exists, purging if asked to
createBucket() {
  BUCKET=$1
  POLICY=$2
  PURGE=$3

  # Purge the bucket, if set & exists
  # Since PURGE is user input, check explicitly for `true`
  if [ $PURGE = true ]; then
    if checkBucketExists $BUCKET; then
      echo "Purging bucket '$BUCKET'."
      set +e # don't exit if this fails
      /usr/bin/mc rm -r --force myminio/$BUCKET
      set -e # reset `e` as active
    else
      echo "Bucket '$BUCKET' does not exist, skipping purge."
    fi
  fi

  # Create the bucket if it does not exist
  if ! checkBucketExists $BUCKET; then
    echo "Creating bucket '$BUCKET'"
    /usr/bin/mc mb myminio/$BUCKET
  else
    echo "Bucket '$BUCKET' already exists."
  fi

  # At this point, the bucket should exist, skip checking for existence
  # Set policy on the bucket
  echo "Setting policy of bucket '$BUCKET' to '$POLICY'."
  /usr/bin/mc policy set $POLICY myminio/$BUCKET

  if checkBucketExists $BUCKET; then
    echo "Successfully created bucket '$BUCKET'"
  fi
}

# Try connecting to Minio instance
scheme=http
connectToMinio $scheme

# Create the bucket
createBucket examples public false
