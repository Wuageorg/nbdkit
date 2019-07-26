/* nbdkit
 * Copyright (C) 2018-2019 Red Hat Inc.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are
 * met:
 *
 * * Redistributions of source code must retain the above copyright
 * notice, this list of conditions and the following disclaimer.
 *
 * * Redistributions in binary form must reproduce the above copyright
 * notice, this list of conditions and the following disclaimer in the
 * documentation and/or other materials provided with the distribution.
 *
 * * Neither the name of Red Hat nor the names of its contributors may be
 * used to endorse or promote products derived from this software without
 * specific prior written permission.
 *
 * THIS SOFTWARE IS PROVIDED BY RED HAT AND CONTRIBUTORS ''AS IS'' AND
 * ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO,
 * THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A
 * PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL RED HAT OR
 * CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
 * SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
 * LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF
 * USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND
 * ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
 * OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT
 * OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF
 * SUCH DAMAGE.
 */

/* For a note on the implementation of this filter, see bucket.c. */

#include <config.h>

#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <inttypes.h>
#include <string.h>
#include <time.h>
#include <sys/time.h>

#include <pthread.h>

#include <nbdkit-filter.h>

#include "cleanup.h"

#include "bucket.h"

/* Per-connection and global limit, both in bits per second, with zero
 * meaning not set / not enforced.  These are only used when reading
 * the command line and initializing the buckets for the first time.
 * They are not involved in dynamic rate adjustment.
 */
static uint64_t connection_rate = 0;
static uint64_t rate = 0;

/* Files for dynamic rate adjustment. */
static char *connection_rate_file = NULL;
static char *rate_file = NULL;

/* Bucket capacity controls the burst rate.  It is expressed as the
 * length of time in "rate-equivalent seconds" that the client can
 * burst for after a period of inactivity.  This could be adjustable
 * in future.
 */
#define BUCKET_CAPACITY 2.0

/* Global read and write buckets. */
static struct bucket read_bucket;
static pthread_mutex_t read_bucket_lock = PTHREAD_MUTEX_INITIALIZER;
static struct bucket write_bucket;
static pthread_mutex_t write_bucket_lock = PTHREAD_MUTEX_INITIALIZER;

/* Per-connection handle. */
struct rate_handle {
  /* Per-connection read and write buckets. */
  struct bucket read_bucket;
  pthread_mutex_t read_bucket_lock;
  struct bucket write_bucket;
  pthread_mutex_t write_bucket_lock;
};

static void
rate_unload (void)
{
  free (connection_rate_file);
  free (rate_file);
}

/* Called for each key=value passed on the command line. */
static int
rate_config (nbdkit_next_config *next, void *nxdata,
             const char *key, const char *value)
{
  if (strcmp (key, "rate") == 0) {
    if (rate > 0) {
      nbdkit_error ("rate set twice on the command line");
      return -1;
    }
    rate = nbdkit_parse_size (value);
    if (rate == -1)
      return -1;
    if (rate == 0) {
      nbdkit_error ("rate cannot be set to 0");
      return -1;
    }
    return 0;
  }
  else if (strcmp (key, "connection-rate") == 0) {
    if (connection_rate > 0) {
      nbdkit_error ("connection-rate set twice on the command line");
      return -1;
    }
    connection_rate = nbdkit_parse_size (value);
    if (connection_rate == -1)
      return -1;
    if (connection_rate == 0) {
      nbdkit_error ("connection-rate cannot be set to 0");
      return -1;
    }
    return 0;
  }
  else if (strcmp (key, "rate-file") == 0) {
    free (rate_file);
    rate_file = nbdkit_absolute_path (value);
    if (rate_file == NULL)
      return -1;
    return 0;
  }
  else if (strcmp (key, "connection-rate-file") == 0) {
    free (connection_rate_file);
    connection_rate_file = nbdkit_absolute_path (value);
    if (connection_rate_file == NULL)
      return -1;
    return 0;
  }
  else
    return next (nxdata, key, value);
}

static int
rate_config_complete (nbdkit_next_config_complete *next, void *nxdata)
{
  /* Initialize the global buckets. */
  bucket_init (&read_bucket, rate, BUCKET_CAPACITY);
  bucket_init (&write_bucket, rate, BUCKET_CAPACITY);

  return next (nxdata);
}

#define rate_config_help \
  "rate=BITSPERSEC                Limit total bandwidth.\n" \
  "connection-rate=BITSPERSEC     Limit per-connection bandwidth.\n" \
  "rate-file=FILENAME             Dynamically adjust total bandwidth.\n" \
  "connection-rate-file=FILENAME  Dynamically adjust per-connection bandwidth."

/* Create the per-connection handle. */
static void *
rate_open (nbdkit_next_open *next, void *nxdata, int readonly)
{
  struct rate_handle *h;

  if (next (nxdata, readonly) == -1)
    return NULL;

  h = malloc (sizeof *h);
  if (h == NULL) {
    nbdkit_error ("malloc: %m");
    return NULL;
  }

  bucket_init (&h->read_bucket, connection_rate, BUCKET_CAPACITY);
  bucket_init (&h->write_bucket, connection_rate, BUCKET_CAPACITY);
  pthread_mutex_init (&h->read_bucket_lock, NULL);
  pthread_mutex_init (&h->write_bucket_lock, NULL);

  return h;
}

/* Free up the per-connection handle. */
static void
rate_close (void *handle)
{
  struct rate_handle *h = handle;

  pthread_mutex_destroy (&h->read_bucket_lock);
  pthread_mutex_destroy (&h->write_bucket_lock);
  free (h);
}

static void
maybe_adjust (const char *file, struct bucket *bucket, pthread_mutex_t *lock)
{
  FILE *fp;
  ssize_t r;
  size_t len = 0;
  CLEANUP_FREE char *line = NULL;
  int64_t new_rate;
  uint64_t old_rate;

  if (!file) return;

  fp = fopen (file, "r");
  if (fp == NULL)
    return; /* this is not an error */

  r = getline (&line, &len, fp);
  fclose (fp);
  if (r == -1) {
    nbdkit_debug ("could not read rate file: %s: %m", file);
    return;
  }

  if (r > 0 && line[r-1] == '\n') line[r-1] = '\0';
  new_rate = nbdkit_parse_size (line);
  if (new_rate == -1)
    return;

  ACQUIRE_LOCK_FOR_CURRENT_SCOPE (lock);
  old_rate = bucket_adjust_rate (bucket, new_rate);

  if (old_rate != new_rate)
    nbdkit_debug ("rate adjusted from %" PRIu64 " to %" PRIi64,
                  old_rate, new_rate);
}

static inline int
maybe_sleep (struct bucket *bucket, pthread_mutex_t *lock, uint32_t count,
             int *err)
{
  struct timespec ts;
  uint64_t bits;

  /* Count is in bytes, but we rate limit using bits.  We could
   * multiply this by 10 to include start/stop but let's not
   * second-guess the transport layers underneath.
   */
  bits = count * UINT64_C(8);

  while (bits > 0) {
    /* Run the token bucket algorithm. */
    {
      ACQUIRE_LOCK_FOR_CURRENT_SCOPE (lock);
      bits = bucket_run (bucket, bits, &ts);
    }

    if (bits > 0)
      if (nanosleep (&ts, NULL) == -1) {
        nbdkit_error ("nanosleep: %m");
        *err = errno;
        return -1;
      }
  }
  return 0;
}

/* Read data. */
static int
rate_pread (struct nbdkit_next_ops *next_ops, void *nxdata,
            void *handle, void *buf, uint32_t count, uint64_t offset,
            uint32_t flags, int *err)
{
  struct rate_handle *h = handle;

  maybe_adjust (rate_file, &read_bucket, &read_bucket_lock);
  if (maybe_sleep (&read_bucket, &read_bucket_lock, count, err))
    return -1;
  maybe_adjust (connection_rate_file, &h->read_bucket, &h->read_bucket_lock);
  if (maybe_sleep (&h->read_bucket, &h->read_bucket_lock, count, err))
    return -1;

  return next_ops->pread (nxdata, buf, count, offset, flags, err);
}

/* Write data. */
static int
rate_pwrite (struct nbdkit_next_ops *next_ops, void *nxdata,
             void *handle,
             const void *buf, uint32_t count, uint64_t offset, uint32_t flags,
             int *err)
{
  struct rate_handle *h = handle;

  maybe_adjust (rate_file, &write_bucket, &write_bucket_lock);
  if (maybe_sleep (&write_bucket, &write_bucket_lock, count, err))
    return -1;
  maybe_adjust (connection_rate_file, &h->write_bucket, &h->write_bucket_lock);
  if (maybe_sleep (&h->write_bucket, &h->write_bucket_lock, count, err))
    return -1;

  return next_ops->pwrite (nxdata, buf, count, offset, flags, err);
}

static struct nbdkit_filter filter = {
  .name              = "rate",
  .longname          = "nbdkit rate filter",
  .version           = PACKAGE_VERSION,
  .unload            = rate_unload,
  .config            = rate_config,
  .config_complete   = rate_config_complete,
  .config_help       = rate_config_help,
  .open              = rate_open,
  .close             = rate_close,
  .pread             = rate_pread,
  .pwrite            = rate_pwrite,
};

NBDKIT_REGISTER_FILTER(filter)
