package main

import (
	"bytes"
	"io"

	"github.com/BurntSushi/csql"
	"github.com/BurntSushi/goim/imdb"
)

// The following are used to identify special strings in a movie line from
// the movies list.
var attrTv, attrVid, attrVg = []byte("(TV)"), []byte("(V)"), []byte("(VG)")
var attrUnknownYear, attrSuspended = []byte("????"), []byte("{{SUSPENDED}}")

func listMovies(db *imdb.DB, movies io.ReadCloser) {
	logf("Reading movies list...")
	addedMovies, addedTvshows, addedEpisodes := 0, 0, 0

	// Postgresql wants different transactions for each inserter.
	// SQLite can't handle them.
	tx1, err := db.Begin()
	csql.SQLPanic(err)
	tx2, err := tx1.Another()
	csql.SQLPanic(err)
	tx3, err := tx1.Another()
	csql.SQLPanic(err)
	tx4, err := tx1.Another()
	csql.SQLPanic(err)

	batchSize := 50
	mvIns, err := db.NewInserter(tx1, batchSize, "movie",
		"id", "title", "year", "sequence", "tv", "video")
	csql.SQLPanic(err)
	tvIns, err := db.NewInserter(tx2, batchSize, "tvshow",
		"id", "title", "year", "sequence", "year_start", "year_end")
	csql.SQLPanic(err)
	epIns, err := db.NewInserter(tx3, batchSize, "episode",
		"id", "tvshow_id", "title", "year", "season", "episode")
	csql.SQLPanic(err)

	atoms, err := db.NewAtomizer(tx4)
	csql.SQLPanic(err)
	listLines(movies, func(line []byte) bool {
		fields := splitListLine(line)
		item, value := fields[0], fields[1]
		switch ent := entityType("movies", item); ent {
		case entityMovie:
			m := imdb.Movie{}
			if existed, err := parseId(atoms, item, &m.Id); existed {
				return true
			} else if err != nil {
				csql.SQLPanic(err)
			}
			if !parseMovie(item, &m) {
				return true
			}
			err := mvIns.Exec(m.Id, m.Title, m.Year, m.Sequence, m.Tv, m.Video)
			if err != nil {
				logf("Full movie info (that failed to add): %#v", m)
				csql.SQLPanic(ef("Could not add movie '%s': %s", m, err))
			}
			addedMovies++
		case entityTvshow:
			tv := imdb.Tvshow{}
			if existed, err := parseId(atoms, item, &tv.Id); existed {
				return true
			} else if err != nil {
				csql.SQLPanic(err)
			}
			if !parseTvshow(item, &tv) {
				return true
			}
			if !parseTvshowRange(value, &tv) {
				return true
			}
			err := tvIns.Exec(tv.Id, tv.Title, tv.Year, tv.Sequence,
				tv.YearStart, tv.YearEnd)
			if err != nil {
				logf("Full tvshow info (that failed to add): %#v", tv)
				csql.SQLPanic(ef("Could not add tvshow '%s': %s", tv, err))
			}
			addedTvshows++
		case entityEpisode:
			ep := imdb.Episode{}
			if existed, err := parseId(atoms, item, &ep.Id); existed {
				return true
			} else if err != nil {
				csql.SQLPanic(err)
			}
			if !parseEpisode(atoms, item, &ep) {
				return true
			}
			if !parseEpisodeYear(value, &ep) {
				return true
			}
			err := epIns.Exec(ep.Id, ep.TvshowId, ep.Title, ep.Year,
				ep.Season, ep.Episode)
			if err != nil {
				logf("Full episode info (that failed to add): %#v", ep)
				csql.SQLPanic(ef("Could not add episode '%s': %s", ep, err))
			}
			addedEpisodes++
		default:
			csql.SQLPanic(ef("Unrecognized entity %s", ent))
		}
		return true
	})
	logf("Done. Added %d movies, %d tv shows and %d episodes.",
		addedMovies, addedTvshows, addedEpisodes)
}

func parseTvshow(tvshow []byte, tv *imdb.Tvshow) bool {
	var field []byte
	fields := bytes.Fields(tvshow)
	for i := len(fields) - 1; i >= 0; i-- {
		field = fields[i]
		if hasEntryYear(field) {
			err := parseEntryYear(field, &tv.Year, &tv.Sequence)
			if err != nil {
				pef("Could not convert '%s' to year: %s", field, err)
				return false
			}
			tv.Title = parseTvshowTitle(bytes.Join(fields[0:i], space))
			return true
		}
	}
	pef("Could not find title in '%s'.", tvshow)
	return false
}

func parseTvshowRange(years []byte, tv *imdb.Tvshow) bool {
	rangeSplit := bytes.Split(years, hypen)
	if err := parseEntryYear(rangeSplit[0], &tv.YearStart, nil); err != nil {
		pef("Could not parse '%s' as year in: %s", rangeSplit[0], years)
		return false
	}
	if len(rangeSplit) > 1 {
		if err := parseEntryYear(rangeSplit[1], &tv.YearEnd, nil); err != nil {
			pef("Could not parse '%s' as year in: %s", rangeSplit[1], years)
			return false
		}
	}
	return true
}

func parseEpisode(az imdb.Atomer, episode []byte, ep *imdb.Episode) bool {
	if episode[len(episode)-1] != '}' {
		pef("Episodes must end with '}' but '%s' does not.", episode)
		return false
	}
	openBrace := bytes.IndexByte(episode, '{')
	if openBrace == -1 {
		pef("Episodes must have a '{..}' but '%s' does not.", episode)
		return false
	}

	var err error
	ep.TvshowId, _, err = az.Atom(episode[0:openBrace])
	if err != nil {
		pef("Could not atomize TV show '%s' from episode '%s': %s",
			episode[0:openBrace], episode, err)
		return false
	}

	inBraces := episode[openBrace+1 : len(episode)-1]
	start := parseEpisodeNumbers(inBraces, &ep.Season, &ep.Episode) // optional
	ep.Title = unicode(bytes.TrimSpace(inBraces[0:start]))
	return true
}

func parseEpisodeNumbers(inBraces []byte, season *int, episode *int) int {
	if inBraces[len(inBraces)-1] != ')' {
		return len(inBraces)
	}
	start := bytes.LastIndex(inBraces, openHash)
	if start == -1 {
		return len(inBraces)
	}
	numbers := inBraces[start+2 : len(inBraces)-1]
	sepi := bytes.IndexByte(numbers, '.')
	if sepi == -1 {
		// assume season 1
		*season = 1
		if err := parseInt(numbers, episode); err != nil {
			pef("Could not parse '%s' as episode number in: %s",
				numbers, inBraces)
		}
		return start // can't read'em, so ignore'em
	}

	sn, en := numbers[:sepi], numbers[sepi+1:]
	if err := parseInt(sn, season); err != nil {
		pef("Could not parse '%s' as season in: %s", sn, inBraces)
	}
	if err := parseInt(en, episode); err != nil {
		pef("Could not parse '%s' as episode number in: %s", en, inBraces)
	}
	return start
}

func parseEpisodeYear(year []byte, ep *imdb.Episode) bool {
	if err := parseEntryYear(year, &ep.Year, nil); err != nil {
		pef("Could not parse '%s' as year.", year)
		return false
	}
	return true
}

func parseMovie(movie []byte, m *imdb.Movie) bool {
	// We start backwards and greedily consume the following attributes:
	//     (YYYY) - The year the movie was released.
	//				Everything after (errm, before) this is the title.
	//	   (TV)   - Made for TV
	//	   (V)    - Made for video
	//	   (VG)   - A video game! Skip it.
	var field []byte
	fields := bytes.Fields(movie)
	for i := len(fields) - 1; i >= 0; i-- {
		field = fields[i]
		switch {
		// Try the common case first.
		case hasEntryYear(field):
			err := parseEntryYear(field[1:len(field)-1], &m.Year, &m.Sequence)
			if err != nil {
				pef("Could not convert '%s' to year: %s", field, err)
				return false
			}
			m.Title = unicode(bytes.Join(fields[0:i], []byte{' '}))
			return true
		case bytes.Equal(field, attrVg):
			return false
		case bytes.Equal(field, attrTv):
			m.Tv = true
		case bytes.Equal(field, attrVid):
			m.Video = true
		}
	}
	pef("Could not find title in '%s'.", movie)
	return false
}

func parseTvshowTitle(quoted []byte) string {
	return unicode(bytes.Trim(bytes.TrimSpace(quoted), "\""))
}
