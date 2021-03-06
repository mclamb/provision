drpcli machines show
--------------------

Show a single machines by id

Synopsis
~~~~~~~~

This will show a machine by ID. You may also show a single item using a
unique index. In that case, format id as *index*:*value*

::

   drpcli machines show [id] [flags]

Options
~~~~~~~

::

         --decode          Should decode any secure params.
         --etag string     etag to use for waiting.
     -h, --help            help for show
         --params string   Should return only the parameters specified as a comma-separated list of parameter names.
         --slim string     Should elide certain fields.  Can be 'Params', 'Meta', or a comma-separated list of both.
         --wait string     The time to wait for an etag change. This is a time string (default units seconds).  Maximum 10m

Options inherited from parent commands
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

::

     -c, --catalog string          The catalog file to use to get product information (default "https://repo.rackn.io")
     -d, --debug                   Whether the CLI should run in debug mode
     -D, --download-proxy string   HTTP Proxy to use for downloading catalog and content
     -E, --endpoint string         The Digital Rebar Provision API endpoint to talk to (default "https://127.0.0.1:8092")
     -f, --force                   When needed, attempt to force the operation - used on some update/patch calls
     -F, --format string           The serialization we expect for output.  Can be "json" or "yaml" or "text" or "table" (default "json")
     -H, --no-header               Should header be shown in "text" or "table" mode
     -x, --noToken                 Do not use token auth or token cache
     -P, --password string         password of the Digital Rebar Provision user (default "r0cketsk8ts")
     -J, --print-fields string     The fields of the object to display in "text" or "table" mode. Comma separated
     -r, --ref string              A reference object for update commands that can be a file name, yaml, or json blob
     -T, --token string            token of the Digital Rebar Provision access
     -t, --trace string            The log level API requests should be logged at on the server side
     -Z, --traceToken string       A token that individual traced requests should report in the server logs
     -j, --truncate-length int     Truncate columns at this length (default 40)
     -u, --url-proxy string        URL Proxy for passing actions through another DRP
     -U, --username string         Name of the Digital Rebar Provision user to talk to (default "rocketskates")

SEE ALSO
~~~~~~~~

-  `drpcli machines <drpcli_machines.html>`__ - Access CLI commands
   relating to machines
