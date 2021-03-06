package bits

import (
	"fmt"
	"io"
	"os"
)

//Get will read and decrypt chunks for keys provided by the key reader and write
//each chunk's contents to chunk writer 'cw' in order of key appearance. Chunks are
//fetched concurrently (locally or remote) but are guaranteed to arrive in
//order to writer 'cw' for assembly in the original format
func Get(kr KeyReader, cw ChunkWriter, conf Config) error {

	//result of working the item
	type result struct {
		chunk []byte
		err   error
	}

	//work item
	type item struct {
		key   K
		resCh chan *result
		err   error
		pos   int64
	}

	//work is run concurrently
	srcs := conf.Stores.GetSrcs()
	work := func(it *item) {

		//ask each key container if it has one
		var chunk []byte
		var err error
		for _, g := range srcs {
			if g == nil {
				continue
			}

			chunk, err = g.Get(it.key)
			if err != nil {
				continue
			}

			break
		}

		if err != nil {
			if os.IsNotExist(err) {
				it.resCh <- &result{nil, ErrNoSuchKey}
				return
			}

			it.resCh <- &result{nil, fmt.Errorf("failed to find key '%s': %v", it.key, err)}
			return
		}

		if len(chunk) < conf.AEAD.NonceSize() {
			it.resCh <- &result{nil, fmt.Errorf("encrypted chunk is too small (must be at least %d long): authentication failed", conf.AEAD.NonceSize())}
			return
		}

		res := &result{}
		res.chunk, res.err = conf.AEAD.Open(nil, chunk[:conf.AEAD.NonceSize()], chunk[conf.AEAD.NonceSize():], nil)
		it.resCh <- res
	}

	//fan-out concurrent work
	itemCh := make(chan *item, conf.GetConcurrency)
	go func() {
		defer close(itemCh)
		pos := int64(0)
		for {
			k, err := kr.Read()
			if err != nil {
				if err != io.EOF {
					itemCh <- &item{
						key: k,
						err: fmt.Errorf("failed to iterate into next key: %v", err),
					}
				}

				break
			}

			it := &item{
				pos:   pos,
				key:   k,
				resCh: make(chan *result),
			}

			go work(it)  //create work
			itemCh <- it //send to fan-in thread for syncing results
			pos++
		}
	}()

	//fan in, output plaintext chunks
	var lastpos int64
	for it := range itemCh {
		if it.err != nil {
			return fmt.Errorf("failed to iterate: %v", it.err)
		}

		if lastpos > it.pos {
			//the language spec is unclear about guaranteed FIFO behaviour of
			//buffered channels, in rare conditions this behaviour might not
			//be guaranteed, for this project such a case be catestropic as it WILL
			//corrupt large files. This is a buildin safeguard that asks the user to
			//submit a real world example if this happens
			return fmt.Errorf("Unexpected race condition during joining, chunk '%d' arrived before chunk '%d', please report this to the author with the file that is being split", lastpos, it.pos)
		}

		res := <-it.resCh
		if res.err != nil {
			return fmt.Errorf("failed to work chunk '%s': %v", it.key, res.err)
		}

		_, err := cw.Write(res.chunk)
		if err != nil {
			return fmt.Errorf("failed to write chunk '%s' to output: %v", it.key, err)
		}

		lastpos = it.pos
	}

	return nil
}
