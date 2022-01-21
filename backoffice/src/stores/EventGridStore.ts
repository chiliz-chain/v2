import {DependencyList, useEffect, useMemo} from "react";
import {LocalGridStore} from "./LocalGridStore";
import {Web3FetchEventLogsFn} from "./ChilizStore";

export const useEventGridStore = <T>(fn: Web3FetchEventLogsFn<T>, deps: DependencyList | undefined = []): LocalGridStore<T> => {
  const store = useMemo(() => {
    return new LocalGridStore<T>(async (offset: number, limit: number): Promise<[T[], boolean]> => {
      return [await fn({
        fromBlock: 0,
        toBlock: 'latest',
      }), false]
    });
    // eslint-disable-next-line
  }, deps)
  useEffect(() => {
    // noinspection JSIgnoredPromiseFromCall
    store.fetchItems()
    return () => store.removeItems()
  }, [store])
  return store
}